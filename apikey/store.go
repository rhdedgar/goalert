package apikey

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/target/goalert/gadb"
	"github.com/target/goalert/graphql2"
	"github.com/target/goalert/keyring"
	"github.com/target/goalert/permission"
	"github.com/target/goalert/util/log"
	"github.com/target/goalert/util/sqlutil"
	"github.com/target/goalert/validation"
	"github.com/target/goalert/validation/validate"
)

// Store is used to manage API keys.
type Store struct {
	db  *sql.DB
	key keyring.Keyring
}

// NewStore will create a new Store.
func NewStore(ctx context.Context, db *sql.DB, key keyring.Keyring) (*Store, error) {
	s := &Store{
		db:  db,
		key: key,
	}

	return s, nil
}

type APIKeyInfo struct {
	ID            uuid.UUID
	Name          string
	Description   string
	ExpiresAt     time.Time
	LastUsed      *APIKeyUsage
	CreatedAt     time.Time
	UpdatedAt     time.Time
	CreatedBy     *uuid.UUID
	UpdatedBy     *uuid.UUID
	AllowedFields []string
}

func (s *Store) FindAllAdminGraphQLKeys(ctx context.Context) ([]APIKeyInfo, error) {
	err := permission.LimitCheckAny(ctx, permission.Admin)
	if err != nil {
		return nil, err
	}

	keys, err := gadb.New(s.db).APIKeyList(ctx)
	if err != nil {
		return nil, err
	}

	res := make([]APIKeyInfo, 0, len(keys))
	for _, k := range keys {
		k := k

		var p GQLPolicy
		err = json.Unmarshal(k.Policy, &p)
		if err != nil {
			log.Log(ctx, fmt.Errorf("invalid policy for key %s: %w", k.ID, err))
			continue
		}
		if p.Version != 1 {
			log.Log(ctx, fmt.Errorf("unknown policy version for key %s: %d", k.ID, p.Version))
			continue
		}

		var lastUsed *APIKeyUsage
		if k.LastUsedAt.Valid {
			var ip string
			if k.LastIpAddress.Valid {
				ip = k.LastIpAddress.IPNet.IP.String()
			}
			lastUsed = &APIKeyUsage{
				UserAgent: k.LastUserAgent.String,
				IP:        ip,
				Time:      k.LastUsedAt.Time,
			}
		}

		res = append(res, APIKeyInfo{
			ID:            k.ID,
			Name:          k.Name,
			Description:   k.Description,
			ExpiresAt:     k.ExpiresAt,
			LastUsed:      lastUsed,
			CreatedAt:     k.CreatedAt,
			UpdatedAt:     k.UpdatedAt,
			CreatedBy:     &k.CreatedBy.UUID,
			UpdatedBy:     &k.UpdatedBy.UUID,
			AllowedFields: p.AllowedFields,
		})
	}

	return res, nil
}

type APIKeyUsage struct {
	UserAgent string
	IP        string
	Time      time.Time
}

type UpdateKey struct {
	ID          uuid.UUID
	Name        string
	Description string
}

func (s *Store) UpdateAdminGraphQLKey(ctx context.Context, id uuid.UUID, name, desc *string) error {
	err := permission.LimitCheckAny(ctx, permission.Admin)
	if err != nil {
		return err
	}

	if name != nil {
		err = validate.IDName("Name", *name)
	}
	if desc != nil {
		err = validate.Many(err, validate.Text("Description", *desc, 0, 255))
	}
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer sqlutil.Rollback(ctx, "UpdateAdminGraphQLKey", tx)

	key, err := gadb.New(tx).APIKeyForUpdate(ctx, id)
	if err != nil {
		return err
	}
	if name != nil {
		key.Name = *name
	}
	if desc != nil {
		key.Description = *desc
	}

	var user uuid.NullUUID
	if u, err := uuid.Parse(permission.UserID(ctx)); err == nil {
		user = uuid.NullUUID{UUID: u, Valid: true}
	}

	err = gadb.New(tx).APIKeyUpdate(ctx, gadb.APIKeyUpdateParams{
		ID:          id,
		Name:        key.Name,
		Description: key.Description,
		UpdatedBy:   user,
	})
	if err != nil {
		return err
	}

	return tx.Commit()
}

func (s *Store) DeleteAdminGraphQLKey(ctx context.Context, id uuid.UUID) error {
	err := permission.LimitCheckAny(ctx, permission.Admin)
	if err != nil {
		return err
	}

	var byID uuid.NullUUID
	if id, err := uuid.Parse(permission.UserID(ctx)); err == nil {
		byID = uuid.NullUUID{UUID: id, Valid: true}
	}

	return gadb.New(s.db).APIKeyDelete(ctx, gadb.APIKeyDeleteParams{
		DeletedBy: byID,
		ID:        id,
	})
}

func (s *Store) AuthorizeGraphQL(ctx context.Context, tok, ua, ip string) (context.Context, error) {
	var claims Claims
	_, err := s.key.VerifyJWT(tok, &claims, Issuer, Audience)
	if err != nil {
		return ctx, permission.Unauthorized()
	}
	id, err := uuid.Parse(claims.Subject)
	if err != nil {
		log.Logf(ctx, "apikey: invalid subject: %v", err)
		return ctx, permission.Unauthorized()
	}

	info, valid, err := s._fetchPolicyInfo(ctx, id)
	if err != nil {
		return nil, err
	}
	if !valid {
		// Successful negative cache lookup, we return Unauthorized because although the token was validated, the key was revoked/removed.
		return ctx, permission.Unauthorized()
	}
	if !bytes.Equal(info.Hash, claims.PolicyHash) {
		// We want to log this as a warning, because it is a potential security issue.
		log.Log(ctx, fmt.Errorf("apikey: policy hash mismatch for key %s", id))
		return ctx, permission.Unauthorized()
	}

	err = s._updateLastUsed(ctx, id, ua, ip)
	if err != nil {
		// Recording usage is not critical, so we log the error and continue.
		log.Log(ctx, err)
	}

	ctx = permission.SourceContext(ctx, &permission.SourceInfo{
		ID:   id.String(),
		Type: permission.SourceTypeGQLAPIKey,
	})
	ctx = permission.UserContext(ctx, "", info.Policy.Role)

	ctx = ContextWithPolicy(ctx, &info.Policy)
	return ctx, nil
}

// NewAdminGQLKeyOpts is used to create a new GraphQL API key.
type NewAdminGQLKeyOpts struct {
	Name    string
	Desc    string
	Fields  []string
	Expires time.Time
	Role    permission.Role
}

// CreateAdminGraphQLKey will create a new GraphQL API key returning the ID and token.
func (s *Store) CreateAdminGraphQLKey(ctx context.Context, opt NewAdminGQLKeyOpts) (uuid.UUID, string, error) {
	err := permission.LimitCheckAny(ctx, permission.Admin)
	if err != nil {
		return uuid.Nil, "", err
	}

	err = validate.Many(
		validate.IDName("Name", opt.Name),
		validate.Text("Description", opt.Desc, 0, 255),
		validate.Range("Fields", len(opt.Fields), 1, len(graphql2.SchemaFields())),
		validate.OneOf("Role", opt.Role, permission.RoleAdmin, permission.RoleUser),
	)
	if time.Until(opt.Expires) <= 0 {
		err = validate.Many(err, validation.NewFieldError("Expires", "must be in the future"))
	}
	for i, f := range opt.Fields {
		if slices.Contains(graphql2.SchemaFields(), f) {
			continue
		}

		err = validate.Many(err, validation.NewFieldError(fmt.Sprintf("Fields[%d]", i), "is not a valid field"))
	}
	if err != nil {
		return uuid.Nil, "", err
	}

	sort.Strings(opt.Fields)
	policyData, err := json.Marshal(GQLPolicy{
		Version:       1,
		AllowedFields: opt.Fields,
		Role:          opt.Role,
	})
	if err != nil {
		return uuid.Nil, "", err
	}

	var user uuid.NullUUID
	userID, err := uuid.Parse(permission.UserID(ctx))
	if err == nil {
		user = uuid.NullUUID{UUID: userID, Valid: true}
	}

	id := uuid.New()
	err = gadb.New(s.db).APIKeyInsert(ctx, gadb.APIKeyInsertParams{
		ID:          id,
		Name:        opt.Name,
		Description: opt.Desc,
		ExpiresAt:   opt.Expires,
		Policy:      policyData,
		CreatedBy:   user,
		UpdatedBy:   user,
	})
	if err != nil {
		return uuid.Nil, "", err
	}

	hash := sha256.Sum256([]byte(policyData))
	tok, err := s.key.SignJWT(NewGraphQLClaims(id, hash[:], opt.Expires))
	if err != nil {
		return uuid.Nil, "", err
	}

	return id, tok, nil
}

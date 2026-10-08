// Package keys creates, lists, revokes and authenticates gateway API keys. Only a SHA-256 of a key is
// stored; the full key is shown once, at creation.
package keys

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/abdullah-9211/spillway/internal/db"
	"github.com/abdullah-9211/spillway/internal/db/sqlcgen"
	"github.com/abdullah-9211/spillway/internal/money"
)

const (
	keyPrefix = "spw_"
	prefixLen = 8 // characters shown in lists: "spw_" plus four of the random part
)

var (
	// ErrInvalidKey covers a missing, unknown or revoked key; callers must not tell these apart.
	ErrInvalidKey = errors.New("invalid api key")
	ErrNotFound   = errors.New("api key not found")
	ErrAmbiguous  = errors.New("more than one key matches")
)

type Key struct {
	ID               uuid.UUID
	Name             string
	Prefix           string
	RateLimitRPM     *int
	MonthlyBudget    *money.Micros
	SemanticCache    bool
	CacheNonzeroTemp bool
	CreatedAt        time.Time
	RevokedAt        *time.Time
}

type Store struct {
	q *sqlcgen.Queries
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{q: sqlcgen.New(pool)} }

// Generate returns a new random key and its hash.
func Generate() (full string, hash []byte, err error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	full = keyPrefix + hex.EncodeToString(b)
	return full, Hash(full), nil
}

func Hash(full string) []byte {
	h := sha256.Sum256([]byte(full))
	return h[:]
}

type CreateParams struct {
	Name             string
	RateLimitRPM     *int
	MonthlyBudget    *money.Micros
	SemanticCache    bool
	CacheNonzeroTemp bool
}

// Create stores a new key and returns it with the full secret, which cannot be recovered later.
func (s *Store) Create(ctx context.Context, p CreateParams) (Key, string, error) {
	if err := ValidateName(p.Name); err != nil {
		return Key{}, "", err
	}
	if err := ValidateLimits(p.RateLimitRPM, p.MonthlyBudget); err != nil {
		return Key{}, "", err
	}
	full, hash, err := Generate()
	if err != nil {
		return Key{}, "", err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Key{}, "", err
	}
	arg := sqlcgen.CreateAPIKeyParams{
		ID: id, Name: p.Name, KeyPrefix: full[:prefixLen], KeyHash: hash,
		MonthlyBudgetUsd: db.NullNumericFromMicros(p.MonthlyBudget),
		SemanticCache:    p.SemanticCache, CacheNonzeroTemp: p.CacheNonzeroTemp,
	}
	if p.RateLimitRPM != nil {
		arg.RateLimitRpm = pgtype.Int4{Int32: int32(*p.RateLimitRPM), Valid: true}
	}
	created, err := s.q.CreateAPIKey(ctx, arg)
	if err != nil {
		return Key{}, "", fmt.Errorf("create key: %w", err)
	}
	return Key{ID: id, Name: p.Name, Prefix: full[:prefixLen], RateLimitRPM: p.RateLimitRPM,
		MonthlyBudget: p.MonthlyBudget, SemanticCache: p.SemanticCache, CacheNonzeroTemp: p.CacheNonzeroTemp, CreatedAt: created}, full, nil
}

// Authenticate resolves a bearer token to an active key.
func (s *Store) Authenticate(ctx context.Context, token string) (Key, error) {
	if !strings.HasPrefix(token, keyPrefix) {
		return Key{}, ErrInvalidKey
	}
	row, err := s.q.GetActiveAPIKeyByHash(ctx, Hash(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return Key{}, ErrInvalidKey
	}
	if err != nil {
		return Key{}, fmt.Errorf("look up key: %w", err)
	}
	return fromRow(row.ID, row.Name, row.KeyPrefix, row.RateLimitRpm, row.MonthlyBudgetUsd,
		row.SemanticCache, row.CacheNonzeroTemp, row.CreatedAt, row.RevokedAt)
}

func (s *Store) List(ctx context.Context) ([]Key, error) {
	rows, err := s.q.ListAPIKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	out := make([]Key, 0, len(rows))
	for _, r := range rows {
		k, err := fromRow(r.ID, r.Name, r.KeyPrefix, r.RateLimitRpm, r.MonthlyBudgetUsd,
			r.SemanticCache, r.CacheNonzeroTemp, r.CreatedAt, r.RevokedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, nil
}

// Revoke revokes the key with this id, or with this prefix when exactly one key has it.
func (s *Store) Revoke(ctx context.Context, idOrPrefix string) (Key, error) {
	all, err := s.List(ctx)
	if err != nil {
		return Key{}, err
	}
	var match []Key
	for _, k := range all {
		if k.ID.String() == idOrPrefix || k.Prefix == idOrPrefix {
			match = append(match, k)
		}
	}
	switch {
	case len(match) == 0:
		return Key{}, ErrNotFound
	case len(match) > 1:
		return Key{}, ErrAmbiguous
	}
	k := match[0]
	if _, err := s.q.RevokeAPIKey(ctx, k.ID); errors.Is(err, pgx.ErrNoRows) {
		return k, nil // already revoked
	} else if err != nil {
		return Key{}, fmt.Errorf("revoke key: %w", err)
	}
	now := time.Now().UTC()
	k.RevokedAt = &now
	return k, nil
}

func fromRow(id uuid.UUID, name, prefix string, rpm pgtype.Int4, budget pgtype.Numeric,
	semantic, nonzero bool, created time.Time, revoked *time.Time) (Key, error) {
	b, err := db.NullMicrosFromNumeric(budget)
	if err != nil {
		return Key{}, err
	}
	k := Key{ID: id, Name: name, Prefix: prefix, MonthlyBudget: b, SemanticCache: semantic,
		CacheNonzeroTemp: nonzero, CreatedAt: created, RevokedAt: revoked}
	if rpm.Valid {
		v := int(rpm.Int32)
		k.RateLimitRPM = &v
	}
	return k, nil
}

// SetSemanticCache turns the semantic cache on or off for the key behind a full key string. It exists so
// the cache can be exercised before the admin API (Phase 5) edits this flag.
func (s *Store) SetSemanticCache(ctx context.Context, token string, on bool) (Key, error) {
	if _, err := s.q.SetSemanticCache(ctx, sqlcgen.SetSemanticCacheParams{KeyHash: Hash(token), SemanticCache: on}); err != nil {
		return Key{}, fmt.Errorf("set semantic cache: %w", err)
	}
	return s.Authenticate(ctx, token)
}

// BuiltinName is the key the dashboard's playground uses (created in Phase 7). It cannot be created, edited or
// revoked through the admin API.
const BuiltinName = "playground"

const (
	MaxNameLen   = 64
	MaxRPM       = 1_000_000
	MaxBudgetUSD = money.Micros(1_000_000 * 1_000_000) // $1,000,000 a month
)

var ErrInvalid = errors.New("invalid key settings")

func invalidf(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

func ValidateName(name string) error {
	n := strings.TrimSpace(name)
	switch {
	case n == "":
		return invalidf("a key needs a name")
	case len(n) > MaxNameLen:
		return invalidf("the name can be at most %d characters", MaxNameLen)
	case strings.EqualFold(n, BuiltinName):
		return invalidf("%q is reserved for the dashboard's playground", BuiltinName)
	}
	return nil
}

func ValidateLimits(rpm *int, budget *money.Micros) error {
	if rpm != nil && (*rpm < 1 || *rpm > MaxRPM) {
		return invalidf("requests a minute must be between 1 and %d", MaxRPM)
	}
	if budget != nil && (*budget < 0 || *budget > MaxBudgetUSD) {
		return invalidf("the monthly budget must be between $0 and $1,000,000")
	}
	return nil
}

// Stats are a key plus what the admin screens show next to it.
type Stats struct {
	Key
	Spend      money.Micros // this calendar month, UTC
	LastUsedAt *time.Time
}

// ListWithStats lists every key, active first, with spend since the start of the month containing now.
func (s *Store) ListWithStats(ctx context.Context, now time.Time) ([]Stats, error) {
	n := now.UTC()
	since := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC)
	rows, err := s.q.ListAPIKeysWithStats(ctx, pgtype.Date{Time: since, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	out := make([]Stats, 0, len(rows))
	for _, r := range rows {
		k, err := fromRow(r.ID, r.Name, r.KeyPrefix, r.RateLimitRpm, r.MonthlyBudgetUsd, r.SemanticCache, r.CacheNonzeroTemp, r.CreatedAt, r.RevokedAt)
		if err != nil {
			return nil, err
		}
		spend, err := db.MicrosFromNumeric(r.SpendUsd)
		if err != nil {
			return nil, err
		}
		st := Stats{Key: k, Spend: spend}
		if r.LastUsedAt.Unix() > 0 {
			t := r.LastUsedAt.UTC()
			st.LastUsedAt = &t
		}
		out = append(out, st)
	}
	return out, nil
}

func (s *Store) Get(ctx context.Context, id uuid.UUID) (Key, error) {
	r, err := s.q.GetAPIKeyByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Key{}, ErrNotFound
	}
	if err != nil {
		return Key{}, fmt.Errorf("get key: %w", err)
	}
	return fromRow(r.ID, r.Name, r.KeyPrefix, r.RateLimitRpm, r.MonthlyBudgetUsd, r.SemanticCache, r.CacheNonzeroTemp, r.CreatedAt, r.RevokedAt)
}

// Optional says whether a field was present in an update and, if so, its new value (nil clears a limit).
type Optional[T any] struct {
	Set   bool
	Value *T
}

type UpdateParams struct {
	Name             *string
	RateLimitRPM     Optional[int]
	MonthlyBudget    Optional[money.Micros]
	SemanticCache    *bool
	CacheNonzeroTemp *bool
}

var ErrRevoked = errors.New("the key is revoked")

// Update changes the fields that are set. A revoked key cannot be edited.
func (s *Store) Update(ctx context.Context, id uuid.UUID, p UpdateParams) (Key, error) {
	cur, err := s.Get(ctx, id)
	if err != nil {
		return Key{}, err
	}
	if cur.Name == BuiltinName {
		return Key{}, invalidf("the playground key is managed by the dashboard")
	}
	if cur.RevokedAt != nil {
		return Key{}, ErrRevoked
	}
	if p.Name != nil {
		if err := ValidateName(*p.Name); err != nil {
			return Key{}, err
		}
		n := strings.TrimSpace(*p.Name)
		p.Name = &n
	}
	if err := ValidateLimits(p.RateLimitRPM.Value, p.MonthlyBudget.Value); err != nil {
		return Key{}, err
	}
	arg := sqlcgen.UpdateAPIKeyParams{
		ID: id, SetRpm: p.RateLimitRPM.Set, SetBudget: p.MonthlyBudget.Set,
		MonthlyBudgetUsd: db.NullNumericFromMicros(p.MonthlyBudget.Value),
	}
	if p.Name != nil {
		arg.Name = pgtype.Text{String: *p.Name, Valid: true}
	}
	if p.SemanticCache != nil {
		arg.SemanticCache = pgtype.Bool{Bool: *p.SemanticCache, Valid: true}
	}
	if p.CacheNonzeroTemp != nil {
		arg.CacheNonzeroTemp = pgtype.Bool{Bool: *p.CacheNonzeroTemp, Valid: true}
	}
	if p.RateLimitRPM.Value != nil {
		arg.RateLimitRpm = pgtype.Int4{Int32: int32(*p.RateLimitRPM.Value), Valid: true}
	}
	if _, err := s.q.UpdateAPIKey(ctx, arg); errors.Is(err, pgx.ErrNoRows) {
		return Key{}, ErrRevoked // revoked between the read and the write
	} else if err != nil {
		return Key{}, fmt.Errorf("update key: %w", err)
	}
	return s.Get(ctx, id)
}

// RevokeByID revokes a key. Revoking twice is a no-op. The playground key cannot be revoked.
func (s *Store) RevokeByID(ctx context.Context, id uuid.UUID) (Key, error) {
	cur, err := s.Get(ctx, id)
	if err != nil {
		return Key{}, err
	}
	if cur.Name == BuiltinName {
		return Key{}, invalidf("the playground key is managed by the dashboard")
	}
	if _, err := s.q.RevokeAPIKey(ctx, id); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Key{}, fmt.Errorf("revoke key: %w", err)
	}
	return s.Get(ctx, id)
}

// EnsureBuiltin makes sure the dashboard's playground key exists with the configured limits. Nobody holds its secret:
// its hash comes from random bytes that are thrown away, so it cannot be used from outside; the playground calls
// the gateway with it directly. Limits from the config replace whatever is stored, so editing the file takes effect.
func (s *Store) EnsureBuiltin(ctx context.Context, rpm *int, budget *money.Micros) (Key, error) {
	if err := ValidateLimits(rpm, budget); err != nil {
		return Key{}, err
	}
	row, err := s.q.GetAPIKeyByName(ctx, BuiltinName)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		id, err := uuid.NewV7()
		if err != nil {
			return Key{}, err
		}
		secret, hash, err := Generate()
		if err != nil {
			return Key{}, err
		}
		arg := sqlcgen.CreateAPIKeyParams{ID: id, Name: BuiltinName, KeyPrefix: "spw_play", KeyHash: hash, MonthlyBudgetUsd: db.NullNumericFromMicros(budget)}
		_ = secret // discarded on purpose
		if rpm != nil {
			arg.RateLimitRpm = pgtype.Int4{Int32: int32(*rpm), Valid: true}
		}
		if _, err := s.q.CreateAPIKey(ctx, arg); err != nil {
			return Key{}, fmt.Errorf("create playground key: %w", err)
		}
		return s.Get(ctx, id)
	case err != nil:
		return Key{}, fmt.Errorf("find playground key: %w", err)
	}
	arg := sqlcgen.SetAPIKeyLimitsParams{ID: row.ID, MonthlyBudgetUsd: db.NullNumericFromMicros(budget)}
	if rpm != nil {
		arg.RateLimitRpm = pgtype.Int4{Int32: int32(*rpm), Valid: true}
	}
	if err := s.q.SetAPIKeyLimits(ctx, arg); err != nil {
		return Key{}, fmt.Errorf("update playground key: %w", err)
	}
	return s.Get(ctx, row.ID)
}

// Stats returns one key with this month's spend.
func (s *Store) Stats(ctx context.Context, id uuid.UUID, now time.Time) (Stats, error) {
	all, err := s.ListWithStats(ctx, now)
	if err != nil {
		return Stats{}, err
	}
	for _, st := range all {
		if st.ID == id {
			return st, nil
		}
	}
	return Stats{}, ErrNotFound
}

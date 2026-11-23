package acyclic

// Conversation persistence. Implement ConversationStore over your own
// database, or use SQLStore with Postgres.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
)

// ConversationStore saves conversations between runs. Load returns nil, nil
// for an unknown id.
type ConversationStore interface {
	Load(ctx context.Context, id string) ([]Message, error)
	Save(ctx context.Context, id string, messages []Message) error
	Delete(ctx context.Context, id string) error
}

type memoryStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

// MemoryStore keeps conversations in process memory (tests, demos).
func MemoryStore() ConversationStore { return &memoryStore{m: map[string][]byte{}} }

func (s *memoryStore) Load(_ context.Context, id string) ([]Message, error) {
	s.mu.Lock()
	b, ok := s.m[id]
	s.mu.Unlock()
	if !ok {
		return nil, nil
	}
	var out []Message
	return out, json.Unmarshal(b, &out)
}

func (s *memoryStore) Save(_ context.Context, id string, messages []Message) error {
	b, err := json.Marshal(messages)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.m[id] = b
	s.mu.Unlock()
	return nil
}

func (s *memoryStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	delete(s.m, id)
	s.mu.Unlock()
	return nil
}

// PostgresSchema is the table SQLStore uses. Messages hold PHI: keep the
// database inside your HIPAA boundary, encrypted at rest.
const PostgresSchema = `create table if not exists acyclic_conversations (
  id text primary key,
  messages jsonb not null,
  updated timestamptz not null default now()
);`

type sqlStore struct{ db *sql.DB }

// SQLStore is a ConversationStore over database/sql with PostgresSchema and
// $n placeholders (lib/pq, pgx stdlib). Bring your own driver.
func SQLStore(db *sql.DB) ConversationStore { return &sqlStore{db} }

func (s *sqlStore) Load(ctx context.Context, id string) ([]Message, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `select messages from acyclic_conversations where id = $1`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Message
	return out, json.Unmarshal(raw, &out)
}

func (s *sqlStore) Save(ctx context.Context, id string, messages []Message) error {
	b, err := json.Marshal(messages)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `insert into acyclic_conversations (id, messages, updated) values ($1, $2, now())
on conflict (id) do update set messages = excluded.messages, updated = now()`, id, string(b))
	return err
}

func (s *sqlStore) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `delete from acyclic_conversations where id = $1`, id)
	return err
}

package nodedb

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ModelConfig is which language model a node plans with and how to reach
// it.
type ModelConfig struct {
	// Provider is the kind of service, BaseURL where it is if not in its
	// usual place, and Model the model to ask for.
	Provider, BaseURL, Model string
	// APIKey is what the service wants by way of a key, if anything.
	APIKey string
}

// ModelConfig returns the node's model configuration. found is false if
// none has been set.
func (db *DB) ModelConfig() (cfg ModelConfig, found bool, err error) {
	err = db.sql.QueryRow(`SELECT provider, base_url, model, api_key FROM model_config WHERE singleton_id = 1`).
		Scan(&cfg.Provider, &cfg.BaseURL, &cfg.Model, &cfg.APIKey)
	if errors.Is(err, sql.ErrNoRows) {
		return ModelConfig{}, false, nil
	}
	if err != nil {
		return ModelConfig{}, false, fmt.Errorf("load model configuration: %w", err)
	}
	return cfg, true, nil
}

// SetModelConfig replaces the node's model configuration.
func (db *DB) SetModelConfig(cfg ModelConfig) error {
	return db.durably("save model configuration", func(b *batch) {
		b.exec(`INSERT INTO model_config (singleton_id, provider, base_url, model, api_key) VALUES (1, ?, ?, ?, ?)
			ON CONFLICT (singleton_id) DO UPDATE SET provider = excluded.provider, base_url = excluded.base_url,
				model = excluded.model, api_key = excluded.api_key`, cfg.Provider, cfg.BaseURL, cfg.Model, cfg.APIKey)
	})
}

// Chat is a conversation with the planner.
type Chat struct {
	ID, Title string
	Created   time.Time
}

// Chats lists the conversations on record, newest first.
func (db *DB) Chats() ([]Chat, error) {
	var chats []Chat
	err := db.read(func(rows *sql.Rows) error {
		var c Chat
		var created int64
		if err := rows.Scan(&c.ID, &c.Title, &created); err != nil {
			return err
		}
		c.Created = moment(created)
		chats = append(chats, c)
		return nil
	}, `SELECT chat_id, title, created_at_ns FROM chats ORDER BY created_at_ns DESC, chat_id`)
	if err != nil {
		return nil, fmt.Errorf("load chats: %w", err)
	}
	return chats, nil
}

// CreateChat starts a conversation.
func (db *DB) CreateChat(c Chat) error {
	return db.durably("save chat", func(b *batch) {
		b.exec(`INSERT INTO chats (chat_id, title, created_at_ns) VALUES (?, ?, ?)`, c.ID, c.Title, nanos(c.Created))
	})
}

// DeleteChat forgets a conversation, its messages and its file references
// atomically. It does not unpin blobs: other chats, user pins and jobs may
// still own them; the storage lifecycle must coordinate that separately.
func (db *DB) DeleteChat(id string) error {
	return db.durably("delete chat", func(b *batch) {
		b.exec(`DELETE FROM file_references WHERE owner = ?`, chatFileOwner(id))
		b.exec(`DELETE FROM chats WHERE chat_id = ?`, id)
	})
}

// AppendChatMessages adds messages, each as its JSON, to the end of a
// conversation, all of them or none.
func (db *DB) AppendChatMessages(chatID string, messages []string, now time.Time) error {
	return db.durably("save chat messages", func(b *batch) {
		for _, message := range messages {
			b.exec(`INSERT INTO chat_messages (chat_id, seq, at_ns, message)
				VALUES (?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM chat_messages WHERE chat_id = ?), ?, ?)`,
				chatID, chatID, nanos(now), message)
		}
	})
}

// ChatMessages returns what was said in a conversation, in order, each
// message as its JSON.
func (db *DB) ChatMessages(chatID string) ([]string, error) {
	var messages []string
	err := db.read(func(rows *sql.Rows) error {
		var message string
		if err := rows.Scan(&message); err != nil {
			return err
		}
		messages = append(messages, message)
		return nil
	}, `SELECT message FROM chat_messages WHERE chat_id = ? ORDER BY seq`, chatID)
	if err != nil {
		return nil, fmt.Errorf("load chat messages: %w", err)
	}
	return messages, nil
}

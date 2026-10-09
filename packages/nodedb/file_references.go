package nodedb

import (
	"database/sql"
	"fmt"
)

// RetainChatFile assigns an uploaded file to an existing conversation.
// The caller supplies authenticated upload metadata, never parsed model text.
// Checking the chat and recording ownership share a transaction with deletion.
func (db *DB) RetainChatFile(chatID, cid string) error {
	return db.durably("retain chat file", func(b *batch) {
		var exists int
		b.err = b.tx.QueryRow(`SELECT 1 FROM chats WHERE chat_id = ?`, chatID).Scan(&exists)
		b.exec(`INSERT INTO file_references(owner, cid) VALUES (?, ?) ON CONFLICT(owner, cid) DO NOTHING`, chatFileOwner(chatID), cid)
	})
}

// chatFileOwner is a reserved namespace used only for explicit chat ownership.
func chatFileOwner(chatID string) string { return "chat:" + chatID }

// AddFileReference records an explicit owner of a file already on record.
// An owner is an opaque ID for a chat, upload draft or explicit user pin.
// Repeating the same reference is idempotent. Do not derive ownership from
// model/user-authored attachment text: the upload lifecycle assigns it.
func (db *DB) AddFileReference(cid, owner string) error {
	return db.durably("retain file reference", func(b *batch) {
		b.exec(`INSERT INTO file_references(owner, cid) VALUES (?, ?) ON CONFLICT(owner, cid) DO NOTHING`, owner, cid)
	})
}

// RemoveFileReference releases only this owner's reference, not other
// owners or the file itself. Missing references are harmless.
func (db *DB) RemoveFileReference(cid, owner string) error {
	return db.durably("release file reference", func(b *batch) {
		b.exec(`DELETE FROM file_references WHERE owner = ? AND cid = ?`, owner, cid)
	})
}

// FileReferences lists the explicit owners, in stable order. An empty
// list is not permission to unpin/delete a blob: existing files may have
// legacy user pins and jobs own their blobs separately. Storage cleanup
// must coordinate those pins and concurrent uploads as well.
func (db *DB) FileReferences(cid string) ([]string, error) {
	owners := []string{}
	err := db.read(func(rows *sql.Rows) error {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return err
		}
		owners = append(owners, owner)
		return nil
	}, `SELECT owner FROM file_references WHERE cid = ? ORDER BY owner`, cid)
	if err != nil {
		return nil, fmt.Errorf("load file references: %w", err)
	}
	return owners, nil
}

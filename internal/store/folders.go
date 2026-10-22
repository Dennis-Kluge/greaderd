package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Folder groups feeds. Google Reader calls folders labels.
type Folder struct {
	ID   int64
	Name string
	// Unread is the number of unread entries in the folder's feeds.
	Unread int
}

// Folders returns all folders ordered by name, with unread counts.
func (s *Store) Folders(ctx context.Context) ([]Folder, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT fo.id, fo.name,
		       (SELECT COUNT(*) FROM entries e JOIN feeds f ON f.id = e.feed_id
		        WHERE f.folder_id = fo.id AND e.is_read = 0)
		FROM folders fo
		ORDER BY fo.name COLLATE NOCASE`)
	if err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	folders, err := collect(rows, func(sc scanner) (Folder, error) {
		var f Folder
		err := sc.Scan(&f.ID, &f.Name, &f.Unread)
		return f, err
	})
	if err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	return folders, nil
}

// EnsureFolder returns the ID of the folder called name, creating it if needed.
func (s *Store) EnsureFolder(ctx context.Context, name string) (int64, error) {
	id, err := ensureFolder(ctx, s.db, name)
	if err != nil {
		return 0, fmt.Errorf("ensure folder %q: %w", name, err)
	}
	return id, nil
}

func ensureFolder(ctx context.Context, q querier, name string) (int64, error) {
	if _, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO folders (name) VALUES (?)`, name); err != nil {
		return 0, err
	}
	return folderID(ctx, q, name)
}

func folderID(ctx context.Context, q querier, name string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM folders WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// RenameFolder renames the folder from to to. If a folder called to already
// exists, the two are merged. The default folder keeps its ID in a merge, so it
// can be renamed but never disappears.
func (s *Store) RenameFolder(ctx context.Context, from, to string) error {
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		fromID, err := folderID(ctx, tx, from)
		if err != nil {
			return err
		}
		toID, err := folderID(ctx, tx, to)
		switch {
		case errors.Is(err, ErrNotFound):
			_, err := tx.ExecContext(ctx, `UPDATE folders SET name = ? WHERE id = ?`, to, fromID)
			return err
		case err != nil:
			return err
		case fromID == toID:
			return nil
		case fromID == DefaultFolderID:
			if err := mergeFolder(ctx, tx, toID, fromID); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `UPDATE folders SET name = ? WHERE id = ?`, to, fromID)
			return err
		default:
			return mergeFolder(ctx, tx, fromID, toID)
		}
	})
	if err != nil {
		return fmt.Errorf("rename folder %q to %q: %w", from, to, err)
	}
	return nil
}

// DeleteFolder deletes the folder called name and moves its feeds to the
// default folder. Deleting a folder that does not exist is not an error.
func (s *Store) DeleteFolder(ctx context.Context, name string) error {
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		id, err := folderID(ctx, tx, name)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if id == DefaultFolderID {
			return ErrDefaultFolder
		}
		return mergeFolder(ctx, tx, id, DefaultFolderID)
	})
	if err != nil {
		return fmt.Errorf("delete folder %q: %w", name, err)
	}
	return nil
}

// mergeFolder moves all feeds from src to dst and deletes src.
func mergeFolder(ctx context.Context, tx *sql.Tx, src, dst int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE feeds SET folder_id = ? WHERE folder_id = ?`, dst, src); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM folders WHERE id = ?`, src)
	return err
}

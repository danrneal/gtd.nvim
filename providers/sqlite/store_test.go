package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	_ "github.com/mattn/go-sqlite3"

	"github.com/danrneal/gtd-cli/model"
)

func TestNewStore(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		setupDBPath  func(t *testing.T) string
		wantErr      bool
		verifyTables func(t *testing.T, dbPath string)
	}{
		{
			name: "valid database path creates tables",
			setupDBPath: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "valid.db")
			},
			verifyTables: func(t *testing.T, dbPath string) {
				db, err := sql.Open("sqlite3", dbPath)
				if err != nil {
					t.Fatalf("failed to open db for verification: %v", err)
				}

				defer db.Close()

				rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
				if err != nil {
					t.Fatalf("failed to query tables: %v", err)
				}

				defer rows.Close()

				var gotTables []string
				for rows.Next() {
					var name string
					if err := rows.Scan(&name); err != nil {
						t.Fatalf("failed to scan table name: %v", err)
					}

					if name != "sqlite_sequence" {
						gotTables = append(gotTables, name)
					}
				}

				if err := rows.Err(); err != nil {
					t.Fatalf("failed iterating rows: %v", err)
				}

				wantTables := []string{"items", "lists"}
				if diff := cmp.Diff(wantTables, gotTables); diff != "" {
					t.Errorf("database tables mismatch (-want +got):\n%s", diff)
				}
			},
		},
		{
			name: "invalid database path (non-existent directory)",
			setupDBPath: func(_ *testing.T) string {
				return "/non/existent/path/db.sqlite"
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dbPath := tt.setupDBPath(t)
			logger := slog.New(slog.DiscardHandler)
			store, err := NewStore(t.Context(), dbPath, logger)

			if tt.wantErr {
				if err == nil {
					t.Error("NewStore() expected error, got nil")
				}

				if store != nil {
					t.Error("NewStore() expected nil store on error, got instance")
				}

				return
			}

			if err != nil {
				t.Errorf("NewStore() unexpected error: %v", err)
				return
			}

			if store == nil {
				t.Fatal("NewStore() expected store instance, got nil")
			}

			if maxConns := store.db.Stats().MaxOpenConnections; maxConns != 1 {
				t.Errorf("expected MaxOpenConnections to be 1, got %d", maxConns)
			}

			if tt.verifyTables != nil {
				tt.verifyTables(t, dbPath)
			}
		})
	}
}

func TestCreateList(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		list     *model.List
		setupCtx func() (context.Context, context.CancelFunc)
		wantList *model.List
		wantErr  bool
	}{
		{
			name: "honor provided list id",
			list: &model.List{
				ID:       "provided",
				Name:     "Honored",
				Modified: time.Now(),
			},
			wantList: &model.List{
				ID:     "provided",
				Name:   "Honored",
				Status: model.StatusOpen,
			},
		},
		{
			name: "valid list with external id",
			list: &model.List{
				Name:       "  Work  \n",
				Position:   1,
				Status:     "",
				Modified:   time.Now(),
				ExternalID: new("ext-123"),
			},
			wantList: &model.List{
				Name:       "Work",
				Position:   1,
				Status:     model.StatusOpen,
				ExternalID: new("ext-123"),
			},
		},
		{
			name: "invalid status for new list",
			list: &model.List{
				Name:     "Invalid",
				Status:   model.StatusDeleted,
				Modified: time.Now(),
			},
			wantErr: true,
		},
		{
			name: "invalid list (validation failed)",
			list: &model.List{
				Name:     "",
				Modified: time.Now(),
			},
			wantErr: true,
		},
		{
			name: "canceled context",
			list: &model.List{
				Name:     "Canceled",
				Modified: time.Now(),
			},
			setupCtx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()

				return ctx, cancel
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var (
				ctx    context.Context
				cancel context.CancelFunc
			)

			if tt.setupCtx != nil {
				ctx, cancel = tt.setupCtx()
				defer cancel()
			} else {
				ctx = t.Context()
			}

			tmpDir := t.TempDir()
			dbPath := filepath.Join(tmpDir, "test.db")
			logger := slog.New(slog.DiscardHandler)
			store, err := NewStore(t.Context(), dbPath, logger)
			if err != nil {
				t.Fatalf("failed to create store: %v", err)
			}

			err = store.CreateList(ctx, tt.list)

			if tt.wantErr {
				if err == nil {
					t.Error("CreateList() expected error, got nil")
				}

				return
			}

			if err != nil {
				t.Errorf("CreateList() unexpected error: %v", err)
				return
			}

			db, err := sql.Open("sqlite3", dbPath)
			if err != nil {
				t.Fatalf("failed to open db for verification: %v", err)
			}

			defer db.Close()

			query := `
				SELECT id, name, position, status, external_id
				FROM lists
				WHERE name = ?
			`

			var gotList model.List
			wantName := strings.TrimSpace(tt.list.Name)
			err = db.QueryRow(query, wantName).Scan(
				&gotList.ID,
				&gotList.Name,
				&gotList.Position,
				&gotList.Status,
				&gotList.ExternalID,
			)
			if err != nil {
				t.Fatalf("failed to query list: %v", err)
			}

			opts := []cmp.Option{
				cmpopts.IgnoreFields(model.List{}, "ID"),
			}

			if tt.list.ID != "" && gotList.ID != tt.list.ID {
				t.Errorf("CreateList() ID mismatch: want %q, got %q", tt.list.ID, gotList.ID)
			} else if tt.list.ID == "" && gotList.ID == "" {
				t.Errorf("CreateList() failed to generate an ID for the list")
			}

			if diff := cmp.Diff(tt.wantList, &gotList, opts...); diff != "" {
				t.Errorf("CreateList() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestListLists(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		setupDB   func(t *testing.T, db *sql.DB)
		setupCtx  func() (context.Context, context.CancelFunc)
		wantLists []model.List
		wantErr   bool
	}{
		{
			name:    "empty db",
			setupDB: nil,
		},
		{
			name: "valid list (empty items)",
			setupDB: func(t *testing.T, db *sql.DB) {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, position, modified)
						VALUES (?, ?, ?, ?)
					`, "list-1", "Inbox", 5, time.Now(),
				)

				mustExec(t, db,
					`
						INSERT INTO lists (id, name, position, modified)
						VALUES (?, ?, ?, ?)
					`, "list-2", "Later", 10, time.Now(),
				)
			},
			wantLists: []model.List{
				{
					ID:       "list-1",
					Name:     "Inbox",
					Status:   model.StatusOpen,
					Position: 0,
					Items:    []*model.Item{},
				},
				{
					ID:       "list-2",
					Name:     "Later",
					Status:   model.StatusOpen,
					Position: 1,
					Items:    []*model.Item{},
				},
			},
		},
		{
			name: "valid list (with items)",
			setupDB: func(t *testing.T, db *sql.DB) {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-2", "Work", time.Now(),
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							title,
							description,
							list_id,
							waiting_on,
							tags,
							modified,
							created
						) VALUES (?, ?, ?, ?, ?, '[]', ?, ?)
					`, "item-1", "Task 1", "", "list-2", "", time.Now(), time.Now(),
				)
			},
			wantLists: []model.List{
				{
					ID:     "list-2",
					Name:   "Work",
					Status: model.StatusOpen,
					Items: []*model.Item{
						{
							ID:     "item-1",
							ListID: "list-2",
							Title:  "Task 1",
							Status: model.StatusNotStarted,
							Tags:   []string{},
						},
					},
				},
			},
		},
		{
			name: "valid list (with complex items)",
			setupDB: func(t *testing.T, db *sql.DB) {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-3", "Complex", time.Now(),
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							title,
							description,
							list_id,
							waiting_on,
							tags,
							modified,
							created
						) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
					`, "item-2", "Task 2", "", "list-3", "", `["a", "b"]`, time.Now(), time.Now(),
				)
			},
			wantLists: []model.List{
				{
					ID:     "list-3",
					Name:   "Complex",
					Status: model.StatusOpen,
					Items: []*model.Item{
						{
							ID:     "item-2",
							ListID: "list-3",
							Title:  "Task 2",
							Status: model.StatusNotStarted,
							Tags:   []string{"a", "b"},
						},
					},
				},
			},
		},
		{
			name: "corrupt item data (bad tags)",
			setupDB: func(t *testing.T, db *sql.DB) {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-4", "Broken", time.Now(),
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							title,
							description,
							list_id,
							waiting_on,
							tags,
							modified,
							created
						) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
					`, "item-3", "Task 3", "", "list-4", "", `{badjson`, time.Now(), time.Now(),
				)
			},
			wantErr: true,
		},
		{
			name: "context cancellation",
			setupDB: func(t *testing.T, db *sql.DB) {
				mustExec(t, db,
					`
						INSERT INTO lists (name, modified)
						VALUES (?, ?)
					`, "Inbox", time.Now(),
				)
			},
			setupCtx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()

				return ctx, cancel
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var (
				ctx    context.Context
				cancel context.CancelFunc
			)

			if tt.setupCtx != nil {
				ctx, cancel = tt.setupCtx()
				defer cancel()
			} else {
				ctx = t.Context()
			}

			tmpDir := t.TempDir()
			dbPath := filepath.Join(tmpDir, "test.db")
			logger := slog.New(slog.DiscardHandler)
			store, err := NewStore(t.Context(), dbPath, logger)
			if err != nil {
				t.Fatalf("failed to create store: %v", err)
			}

			db, err := sql.Open("sqlite3", dbPath)
			if err != nil {
				t.Fatalf("failed to open db for setup: %v", err)
			}

			defer db.Close()

			if tt.setupDB != nil {
				tt.setupDB(t, db)
			}

			lists, err := store.ListLists(ctx)

			if tt.wantErr {
				if err == nil {
					t.Error("ListLists() expected error, got nil")
				}

				return
			}

			if err != nil {
				t.Errorf("ListLists() unexpected error: %v", err)
				return
			}

			opts := []cmp.Option{
				cmpopts.IgnoreFields(model.List{}, "Modified"),
				cmpopts.IgnoreFields(model.Item{}, "Modified", "Created"),
			}

			if diff := cmp.Diff(tt.wantLists, lists, opts...); diff != "" {
				t.Errorf("ListLists() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUpdateList(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		setupDB       func(t *testing.T, db *sql.DB) string
		setupList     func(id string) model.List
		setupCurrent  func() *model.List
		setupCtx      func() (context.Context, context.CancelFunc)
		wantList      *model.List
		wantItems     []*model.Item
		wantErr       bool
		wantErrTarget error
	}{
		{
			name: "valid update (rename and reorder)",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Old Name", time.Now(),
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							title,
							description,
							list_id,
							waiting_on,
							position,
							status,
							modified,
							created
						)
						VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
					`, "item-3", "C", "", "list-1", "", 0, "not_started", time.Now(), time.Now(),
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							title,
							description,
							list_id,
							waiting_on,
							position,
							status,
							modified,
							created
						)
						VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
					`, "item-1", "A", "", "list-1", "", 1, "not_started", time.Now(), time.Now(),
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							title,
							description,
							list_id,
							waiting_on,
							position,
							status,
							modified,
							created
						) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
					`, "item-2", "B", "", "list-1", "", 2, "not_started", time.Now(), time.Now(),
				)

				return "list-1"
			},
			setupList: func(id string) model.List {
				list := model.List{
					ID:       id,
					Name:     "  New Name  \n",
					Position: 5,
					Status:   "",
					Modified: time.Now(),
					Items: []*model.Item{
						{
							ID:       "item-3",
							ListID:   id,
							Position: 0,
							Title:    "C",
							Status:   model.StatusNotStarted,
						},
						{
							ID:       "item-2",
							ListID:   id,
							Position: 1,
							Title:    "B",
							Status:   model.StatusNotStarted,
						},
						{
							ID:       "item-1",
							ListID:   id,
							Position: 2,
							Title:    "A",
							Status:   model.StatusNotStarted,
						},
					},
				}

				return list
			},
			setupCurrent: func() *model.List {
				list := &model.List{
					Name: "Old Name",
					Items: []*model.Item{
						{ID: "item-3"},
						{ID: "item-1"},
						{ID: "item-2"},
					},
				}

				return list
			},
			wantList: &model.List{
				ID:       "list-1",
				Name:     "New Name",
				Position: 5,
				Status:   model.StatusOpen,
			},
			wantItems: []*model.Item{
				{
					ID:       "item-3",
					ListID:   "list-1",
					Position: 0,
					Title:    "C",
					Status:   model.StatusNotStarted,
					Tags:     []string{},
				},
				{
					ID:       "item-2",
					ListID:   "list-1",
					Position: 1,
					Title:    "B",
					Status:   model.StatusNotStarted,
					Tags:     []string{},
				},
				{
					ID:       "item-1",
					ListID:   "list-1",
					Position: 2,
					Title:    "A",
					Status:   model.StatusNotStarted,
					Tags:     []string{},
				},
			},
		},
		{
			name: "valid update (optimization skip)",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-opt", "Optimization", time.Now(),
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							title,
							description,
							list_id,
							waiting_on,
							position,
							status,
							tags,
							modified,
							created
						) VALUES (?, ?, ?, ?, ?, ?, ?, '[]', ?, ?)
					`, "item-opt", "Task Opt", "", "list-opt", "", 99, "not_started", time.Now(), time.Now(),
				)

				return "list-opt"
			},
			setupList: func(id string) model.List {
				list := model.List{
					ID:       id,
					Name:     "Optimization",
					Modified: time.Now(),
					Items: []*model.Item{
						{
							ID:       "item-opt",
							ListID:   id,
							Position: 0,
							Title:    "Task Opt",
						},
					},
				}

				return list
			},
			setupCurrent: func() *model.List {
				list := &model.List{
					Name: "Optimization",
					Items: []*model.Item{
						{
							ID:       "item-opt",
							ListID:   "list-opt",
							Position: 0,
						},
					},
				}

				return list
			},
			wantList: &model.List{
				ID:     "list-opt",
				Name:   "Optimization",
				Status: model.StatusOpen,
			},
			wantItems: []*model.Item{
				{
					ID:       "item-opt",
					ListID:   "list-opt",
					Position: 99,
					Title:    "Task Opt",
					Status:   model.StatusNotStarted,
					Tags:     []string{},
				},
			},
		},
		{
			name: "preserve external id when nil",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (
							id,
							name,
							modified,
							external_id
						)
						VALUES (?, ?, ?, ?)
					`, "list-1", "Original", time.Now(), "ext-1",
				)

				return "list-1"
			},
			setupList: func(id string) model.List {
				list := model.List{
					ID:       id,
					Name:     "Updated",
					Modified: time.Now(),
				}

				return list
			},
			wantList: &model.List{
				ID:         "list-1",
				Name:       "Updated",
				Status:     model.StatusOpen,
				ExternalID: new("ext-1"),
			},
		},
		{
			name: "update list by external id",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (
							id,
							name,
							modified,
							external_id
						)
						VALUES (?, ?, ?, ?)
					`, "list-1", "Original Name", time.Now(), "ext-L1",
				)

				return "list-1"
			},
			setupList: func(_ string) model.List {
				list := model.List{
					ExternalID: new("ext-L1"),
					Name:       "Updated Name",
					Status:     model.StatusOpen,
					Modified:   time.Now(),
				}

				return list
			},
			wantList: &model.List{
				ID:         "list-1",
				Name:       "Updated Name",
				Status:     model.StatusOpen,
				ExternalID: new("ext-L1"),
			},
		},

		{
			name: "update item by external id",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (
							id,
							name,
							modified,
							external_id
						)
						VALUES (?, ?, ?, ?)
					`, "list-1", "List 1", time.Now(), "ext-L1",
				)

				mustExec(t, db,
					`
						INSERT INTO lists (
							id,
							name,
							modified
						)
						VALUES (?, ?, ?)
					`, "list-2", "List 2", time.Now(),
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							title,
							description,
							list_id,
							waiting_on,
							modified,
							created,
							external_id
						)
						VALUES (?, ?, ?, ?, ?, ?, ?, ?)
					`, "item-1", "Original Title", "", "list-2", "", time.Now(), time.Now(), "ext-I1",
				)

				return "list-1"
			},
			setupList: func(_ string) model.List {
				list := model.List{
					Name:       "List 1",
					ExternalID: new("ext-L1"),
					Modified:   time.Now(),
					Items: []*model.Item{
						{
							ExternalID: new("ext-I1"),
							Title:      "Updated Title",
						},
					},
				}

				return list
			},
			wantList: &model.List{
				ID:         "list-1",
				Name:       "List 1",
				Status:     model.StatusOpen,
				ExternalID: new("ext-L1"),
			},
			wantItems: []*model.Item{
				{
					ID:             "item-1",
					ListID:         "list-1",
					Title:          "Original Title",
					Status:         model.StatusNotStarted,
					Tags:           []string{},
					ExternalID:     new("ext-I1"),
					ExternalListID: new("ext-L1"),
				},
			},
		},
		{
			name: "soft delete list cascades to hard delete items",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-delete", "To Be Deleted", time.Now(),
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							title,
							description,
							list_id,
							waiting_on,
							position,
							status,
							modified,
							created
						) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
					`, "item-delete", "Cleanup Item", "", "list-delete", "", 0, "not_started", time.Now(), time.Now(),
				)

				return "list-delete"
			},
			setupList: func(id string) model.List {
				list := model.List{
					ID:       id,
					Name:     "Tombstoned List",
					Status:   model.StatusDeleted,
					Modified: time.Now(),
				}

				return list
			},
			wantList: &model.List{
				ID:     "list-delete",
				Name:   "Tombstoned List",
				Status: model.StatusDeleted,
			},
		},
		{
			name: "soft delete with item external id only",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (
							id,
							name,
							modified,
							external_id
						)
						VALUES (?, ?, ?, ?)
					`, "list-delete", "To Be Deleted", time.Now(), "ext-list-delete",
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							title,
							list_id,
							waiting_on,
							modified,
							created
						) VALUES (?, ?, ?, ?, ?, ?)
					`, "item-delete", "Cleanup Item", "list-delete", "", time.Now(), time.Now(),
				)

				return "list-delete"
			},
			setupList: func(_ string) model.List {
				list := model.List{
					ExternalID: new("ext-list-delete"),
					Name:       "Tombstoned List",
					Status:     model.StatusDeleted,
					Modified:   time.Now(),
				}

				return list
			},
			wantList: &model.List{
				ID:         "list-delete",
				Name:       "Tombstoned List",
				Status:     model.StatusDeleted,
				ExternalID: new("ext-list-delete"),
			},
		},
		{
			name: "missing list identifiers",
			setupDB: func(_ *testing.T, _ *sql.DB) string {
				return ""
			},
			setupList: func(_ string) model.List {
				list := model.List{
					Name:     "Headless Update",
					Modified: time.Now(),
				}

				return list
			},
			wantErr: true,
		},
		{
			name: "update list with nonexistent external id",
			setupDB: func(_ *testing.T, _ *sql.DB) string {
				return ""
			},
			setupList: func(_ string) model.List {
				list := model.List{
					ExternalID: new("non-existent-ext-id"),
					Name:       "Headless Update",
					Modified:   time.Now(),
				}

				return list
			},
			wantErr:       true,
			wantErrTarget: ErrNotFound,
		},
		{
			name: "transaction rollback on item failure",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (
							id,
							name,
							status,
							modified
						)
						VALUES (?, ?, ?, ?)
					`, "list-rollback", "Stable Name", "open", time.Now(),
				)

				return "list-rollback"
			},
			setupList: func(id string) model.List {
				list := model.List{
					ID:     id,
					Name:   "Attempted Change",
					Status: model.StatusOpen,
					Items: []*model.Item{
						{
							ID:       "non-existent-item",
							ListID:   id,
							Position: 0,
						},
					},
				}

				return list
			},
			wantList: &model.List{
				Name:   "Stable Name",
				Status: model.StatusOpen,
			},
			wantErr: true,
		},
		{
			name: "invalid list (validation failed)",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Valid", time.Now(),
				)

				return "list-1"
			},
			setupList: func(id string) model.List {
				list := model.List{
					ID:       id,
					Name:     "",
					Modified: time.Now(),
				}

				return list
			},
			wantErr: true,
		},
		{
			name: "nonexistent id",
			setupDB: func(_ *testing.T, _ *sql.DB) string {
				return "non-existent"
			},
			setupList: func(id string) model.List {
				list := model.List{
					ID:       id,
					Name:     "Valid Name",
					Modified: time.Now(),
				}

				return list
			},
			wantErr:       true,
			wantErrTarget: ErrNotFound,
		},
		{
			name: "missing item identifiers",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Valid List", time.Now(),
				)

				return "list-1"
			},
			setupList: func(id string) model.List {
				list := model.List{
					ID:       id,
					Name:     "Valid List",
					Modified: time.Now(),
					Items: []*model.Item{
						{
							Title:    "Headless Item",
							Modified: time.Now(),
						},
					},
				}

				return list
			},
			wantErr: true,
		},
		{
			name: "nonexistent item id",
			setupDB: func(_ *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "List", time.Now(),
				)

				return "list-1"
			},
			setupList: func(id string) model.List {
				list := model.List{
					ID:       id,
					Name:     "List",
					Modified: time.Now(),
					Items: []*model.Item{
						{
							ID:       "missing-item",
							ListID:   id,
							Position: 0,
							Title:    "Valid Title",
							Modified: time.Now(),
						},
					},
				}

				return list
			},
			wantErr: true,
		},
		{
			name: "nonexistent item external id",
			setupDB: func(_ *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "List", time.Now(),
				)

				return "list-1"
			},
			setupList: func(id string) model.List {
				list := model.List{
					ID:       id,
					Name:     "List",
					Modified: time.Now(),
					Items: []*model.Item{
						{
							ExternalID: new("missing-ext-item"),
							ListID:     id,
							Position:   0,
							Title:      "Valid Title",
							Modified:   time.Now(),
						},
					},
				}

				return list
			},
			wantErr:       true,
			wantErrTarget: ErrNotFound,
		},
		{
			name: "context cancellation",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Valid", time.Now(),
				)

				return "list-1"
			},
			setupList: func(id string) model.List {
				list := model.List{
					ID:       id,
					Name:     "Valid Name",
					Modified: time.Now(),
				}

				return list
			},
			setupCtx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()

				return ctx, cancel
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var (
				ctx    context.Context
				cancel context.CancelFunc
			)

			if tt.setupCtx != nil {
				ctx, cancel = tt.setupCtx()
				defer cancel()
			} else {
				ctx = t.Context()
			}

			tmpDir := t.TempDir()
			dbPath := filepath.Join(tmpDir, "test.db")
			logger := slog.New(slog.DiscardHandler)
			store, err := NewStore(t.Context(), dbPath, logger)
			if err != nil {
				t.Fatalf("failed to create store: %v", err)
			}

			db, err := sql.Open("sqlite3", dbPath)
			if err != nil {
				t.Fatalf("failed to open db for setup: %v", err)
			}

			defer db.Close()

			id := tt.setupDB(t, db)
			list := tt.setupList(id)
			currentList := &model.List{
				Name: list.Name,
			}

			if tt.setupCurrent != nil {
				currentList = tt.setupCurrent()
			}

			err = store.UpdateList(ctx, &list, currentList)

			if tt.wantErr {
				if err == nil {
					t.Error("UpdateList() expected error, got nil")
				} else if tt.wantErrTarget != nil && !errors.Is(err, tt.wantErrTarget) {
					t.Errorf("UpdateList() expected error target %v, got: %v", tt.wantErrTarget, err)
				}

				return
			}

			if err != nil {
				t.Errorf("UpdateList() unexpected error: %v", err)
				return
			}

			query := `
					SELECT name, position, status, external_id
					FROM lists
					WHERE id = ?
			`

			var gotList model.List
			err = db.QueryRow(query, id).Scan(
				&gotList.Name,
				&gotList.Position,
				&gotList.Status,
				&gotList.ExternalID,
			)
			if err != nil {
				t.Fatalf("failed to query list: %v", err)
			}

			gotList.ID = id

			if diff := cmp.Diff(*tt.wantList, gotList); diff != "" {
				t.Errorf("UpdateList() mismatch (-want +got):\n%s", diff)
			}

			tx, _ := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			items, err := store.listAllItems(ctx, tx)
			tx.Rollback()
			if err != nil {
				t.Fatalf("failed to list all items: %v", err)
			}

			itemOpts := []cmp.Option{
				cmpopts.IgnoreFields(model.Item{}, "Modified", "Created"),
			}

			if diff := cmp.Diff(tt.wantItems, items, itemOpts...); diff != "" {
				t.Errorf("UpdateList() items mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDeleteList(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		setupDB       func(t *testing.T, db *sql.DB) model.List
		setupCtx      func() (context.Context, context.CancelFunc)
		wantLists     []model.List
		wantItems     []*model.Item
		wantErr       bool
		wantErrTarget error
	}{
		{
			name: "valid delete with cascade",
			setupDB: func(t *testing.T, db *sql.DB) model.List {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "To Delete", time.Now(),
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							title,
							list_id,
							waiting_on,
							tags,
							modified,
							created
						) VALUES (?, ?, ?, ?, '[]', ?, ?)
					`, "item-1", "Linked Item", "list-1", "", time.Now(), time.Now(),
				)

				list := model.List{
					ID: "list-1",
				}

				return list
			},
		},
		{
			name: "delete list by external id",
			setupDB: func(t *testing.T, db *sql.DB) model.List {
				mustExec(t, db,
					`
						INSERT INTO lists (
							id,
							name,
							modified,
							external_id
						)
						VALUES (?, ?, ?, ?)
					`, "list-ext-del", "List to Delete", time.Now(), "ext-del-1",
				)

				list := model.List{
					ExternalID: new("ext-del-1"),
				}

				return list
			},
		},
		{
			name: "delete list missing identifiers",
			setupDB: func(_ *testing.T, _ *sql.DB) model.List {
				list := model.List{
					Name: "Headless List",
				}

				return list
			},
			wantErr: true,
		},
		{
			name: "delete list with nonexistent external id",
			setupDB: func(_ *testing.T, _ *sql.DB) model.List {
				list := model.List{
					ExternalID: new("non-existent-ext"),
				}

				return list
			},
			wantErr:       true,
			wantErrTarget: ErrNotFound,
		},
		{
			name: "nonexistent id",
			setupDB: func(_ *testing.T, _ *sql.DB) model.List {
				list := model.List{
					ID: "non-existent",
				}

				return list
			},
			wantErr:       true,
			wantErrTarget: ErrNotFound,
		},
		{
			name: "context cancellation",
			setupDB: func(t *testing.T, db *sql.DB) model.List {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "To Delete", time.Now(),
				)

				list := model.List{
					ID: "list-1",
				}

				return list
			},
			setupCtx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()

				return ctx, cancel
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var (
				ctx    context.Context
				cancel context.CancelFunc
			)

			if tt.setupCtx != nil {
				ctx, cancel = tt.setupCtx()
				defer cancel()
			} else {
				ctx = t.Context()
			}

			tmpDir := t.TempDir()
			dbPath := filepath.Join(tmpDir, "test.db")
			logger := slog.New(slog.DiscardHandler)
			store, err := NewStore(t.Context(), dbPath, logger)
			if err != nil {
				t.Fatalf("failed to create store: %v", err)
			}

			db, err := sql.Open("sqlite3", dbPath)
			if err != nil {
				t.Fatalf("failed to open db setup: %v", err)
			}

			defer db.Close()

			list := tt.setupDB(t, db)
			err = store.DeleteList(ctx, &list)

			if tt.wantErr {
				if err == nil {
					t.Error("DeleteList() expected error, got nil")
				} else if tt.wantErrTarget != nil && !errors.Is(err, tt.wantErrTarget) {
					t.Errorf("DeleteList() expected error target %v, got: %v", tt.wantErrTarget, err)
				}

				return
			}

			if err != nil {
				t.Errorf("DeleteList() unexpected error: %v", err)
				return
			}

			lists, err := store.ListLists(ctx)
			if err != nil {
				t.Fatalf("failed to get all lists: %v", err)
			}

			if diff := cmp.Diff(tt.wantLists, lists); diff != "" {
				t.Errorf("DeleteList() lists mismatch (-want +got):\n%s", diff)
			}

			tx, _ := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			items, err := store.listAllItems(ctx, tx)
			tx.Rollback()
			if err != nil {
				t.Fatalf("failed to get all items: %v", err)
			}

			if diff := cmp.Diff(tt.wantItems, items); diff != "" {
				t.Errorf("DeleteList() items mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCreateItem(t *testing.T) {
	t.Parallel()

	listModified := time.Date(2009, time.November, 10, 23, 0, 0, 0, time.UTC)

	tests := []struct {
		name          string
		setupDB       func(t *testing.T, db *sql.DB)
		item          *model.Item
		setupCtx      func() (context.Context, context.CancelFunc)
		wantItem      *model.Item
		wantErr       bool
		wantErrTarget error
	}{
		{
			name: "honor provided identifiers",
			setupDB: func(t *testing.T, db *sql.DB) {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Inbox", listModified,
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							list_id,
							title,
							description,
							waiting_on,
							modified,
							created
						) VALUES (?, ?, ?, ?, ?, ?, ?)
					`, "existing-project", "list-1", "Project", "", "", time.Now(), time.Now(),
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							list_id,
							title,
							description,
							waiting_on,
							modified,
							created,
							external_id
						) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
					`, "poison-project", "list-1", "Poison", "", "", time.Now(), time.Now(), "ext-proj-1",
				)
			},
			item: &model.Item{
				ID:                "provided-item",
				ListID:            "list-1",
				ProjectID:         new("existing-project"),
				ExternalProjectID: new("ext-proj-1"),
				Title:             "Honored Item",
				Modified:          time.Now(),
				Created:           time.Now(),
			},
			wantItem: &model.Item{
				ID:        "provided-item",
				ListID:    "list-1",
				ProjectID: new("existing-project"),
				Title:     "Honored Item",
				Status:    model.StatusNotStarted,
				Tags:      []string{},
			},
		},
		{
			name: "valid item",
			setupDB: func(t *testing.T, db *sql.DB) {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Inbox", listModified,
				)
			},
			item: &model.Item{
				ListID:      "list-1",
				Title:       "  Complex Task  ",
				Description: "  Line 1   \n  Line 2   \n    Line 3",
				Status:      model.StatusDone,
				WaitingOn:   "Alice",
				Tags:        []string{"work", "urgent"},
				Modified:    time.Now(),
				Created:     time.Now(),
			},
			wantItem: &model.Item{
				ListID:      "list-1",
				Title:       "Complex Task",
				Description: "Line 1\nLine 2\n  Line 3",
				Status:      model.StatusDone,
				WaitingOn:   "Alice",
				Tags:        []string{"work", "urgent"},
			},
		},
		{
			name: "valid project",
			setupDB: func(t *testing.T, db *sql.DB) {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", model.ListProjects, listModified,
				)
			},
			item: &model.Item{
				ListID:      "list-1",
				Title:       "  Complex Task  ",
				Description: "  Line 1   \n  Line 2   \n    Line 3",
				Status:      model.StatusDone,
				ProjectTag:  new("proj-1"),
				WaitingOn:   "Alice",
				Tags:        []string{"work", "urgent"},
				Modified:    time.Now(),
				Created:     time.Now(),
			},
			wantItem: &model.Item{
				ListID:      "list-1",
				Title:       "Complex Task",
				Description: "Line 1\nLine 2\n  Line 3",
				Status:      model.StatusDone,
				ProjectTag:  new("proj-1"),
				WaitingOn:   "Alice",
				Tags:        []string{"work", "urgent"},
			},
		},
		{
			name: "resolve project id by external project id",
			setupDB: func(t *testing.T, db *sql.DB) {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Inbox", listModified,
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							list_id,
							title,
							description,
							waiting_on,
							modified,
							created,
							external_id
						) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
					`, "existing-project-id", "list-1", "My Project", "", "", time.Now(), time.Now(), "ext-proj-1",
				)
			},
			item: &model.Item{
				ListID:            "list-1",
				Title:             "Task for Project",
				ExternalProjectID: new("ext-proj-1"),
				Modified:          time.Now(),
				Created:           time.Now(),
			},
			wantItem: &model.Item{
				ListID:    "list-1",
				Title:     "Task for Project",
				Status:    model.StatusNotStarted,
				ProjectID: new("existing-project-id"),
				Tags:      []string{},
			},
		},
		{
			name: "resolve project id by project tag",
			setupDB: func(t *testing.T, db *sql.DB) {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Inbox", listModified,
				)

				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-projects", model.ListProjects, listModified,
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							list_id,
							title,
							description,
							waiting_on,
							project_tag,
							modified,
							created
						) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
					`, "existing-project-id", "list-projects", "My Project", "", "", "proj-1", time.Now(), time.Now(),
				)
			},
			item: &model.Item{
				ListID:     "list-1",
				Title:      "Task for Project",
				ProjectTag: new("proj-1"),
				Modified:   time.Now(),
				Created:    time.Now(),
			},
			wantItem: &model.Item{
				ListID:    "list-1",
				Title:     "Task for Project",
				Status:    model.StatusNotStarted,
				ProjectID: new("existing-project-id"),
				Tags:      []string{},
			},
		},
		{
			name: "error resolving project id by missing external project id",
			setupDB: func(t *testing.T, db *sql.DB) {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Inbox", listModified,
				)
			},
			item: &model.Item{
				ListID:            "list-1",
				Title:             "Task for Missing Project",
				ExternalProjectID: new("does-not-exist"),
				Modified:          time.Now(),
				Created:           time.Now(),
			},
			wantItem: &model.Item{
				ListID: "list-1",
				Title:  "Task for Missing Project",
				Status: model.StatusNotStarted,
				Tags:   []string{},
			},
		},
		{
			name: "ignore missing project tag on non-project list",
			setupDB: func(t *testing.T, db *sql.DB) {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Inbox", listModified,
				)
			},
			item: &model.Item{
				ListID:     "list-1",
				Title:      "Task with Missing Tag",
				ProjectTag: new("missing-tag"),
				Modified:   time.Now(),
				Created:    time.Now(),
			},
			wantItem: &model.Item{
				ListID: "list-1",
				Title:  "Task with Missing Tag",
				Status: model.StatusNotStarted,
				Tags:   []string{},
			},
		},
		{
			name: "create item resolving list id by external id",
			setupDB: func(t *testing.T, db *sql.DB) {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified, external_id)
						VALUES (?, ?, ?, ?)
					`, "list-1", "External List", listModified, "ext-L1",
				)
			},
			item: &model.Item{
				ExternalListID: new("ext-L1"),
				Title:          "Resolved Item",
				Status:         model.StatusNotStarted,
				Modified:       time.Now(),
				Created:        time.Now(),
			},
			wantItem: &model.Item{
				ListID: "list-1",
				Title:  "Resolved Item",
				Status: model.StatusNotStarted,
				Tags:   []string{},
			},
		},
		{
			name:    "create item with nonexistent external list id",
			setupDB: func(_ *testing.T, _ *sql.DB) {},
			item: &model.Item{
				ExternalListID: new("non-existent-ext-id"),
				Title:          "Orphan Item",
				Modified:       time.Now(),
			},
			wantErr:       true,
			wantErrTarget: ErrNotFound,
		},
		{
			name: "empty title",
			setupDB: func(t *testing.T, db *sql.DB) {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Inbox", listModified,
				)
			},
			item: &model.Item{
				ListID: "list-1",
				Title:  "",
			},
			wantErr: true,
		},
		{
			name: "cannot create deleted item",
			setupDB: func(t *testing.T, db *sql.DB) {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Inbox", listModified,
				)
			},
			item: &model.Item{
				ListID:   "list-1",
				Title:    "Deleted Task",
				Status:   model.StatusDeleted,
				Modified: time.Now(),
			},
			wantErr: true,
		},
		{
			name:    "canceled context",
			setupDB: nil,
			item: &model.Item{
				ListID:   "list-1",
				Title:    "Canceled",
				Modified: time.Now(),
			},
			setupCtx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()

				return ctx, cancel
			},
			wantErr: true,
		},
		{
			name: "list iteration fails due to corrupted sibling item tags",
			setupDB: func(t *testing.T, db *sql.DB) {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Inbox", listModified,
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							title,
							description,
							list_id,
							waiting_on,
							tags,
							modified,
							created
						) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
					`, "poisoned-item", "Poisoned Item", "", "list-1", "", "{bad-json", time.Now(), time.Now(),
				)
			},
			item: &model.Item{
				ListID:   "list-1",
				Title:    "Valid New Item",
				Modified: time.Now(),
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var (
				ctx    context.Context
				cancel context.CancelFunc
			)

			if tt.setupCtx != nil {
				ctx, cancel = tt.setupCtx()
				defer cancel()
			} else {
				ctx = t.Context()
			}

			tmpDir := t.TempDir()
			dbPath := filepath.Join(tmpDir, "test.db")
			logger := slog.New(slog.DiscardHandler)
			store, err := NewStore(t.Context(), dbPath, logger)
			if err != nil {
				t.Fatalf("failed to create store: %v", err)
			}

			db, err := sql.Open("sqlite3", dbPath)
			if err != nil {
				t.Fatalf("failed to open db for setup: %v", err)
			}

			defer db.Close()

			if tt.setupDB != nil {
				tt.setupDB(t, db)
			}

			err = store.CreateItem(ctx, tt.item, "")

			if tt.wantErr {
				if err == nil {
					t.Error("CreateItem() expected error, got nil")
				} else if tt.wantErrTarget != nil && !errors.Is(err, tt.wantErrTarget) {
					t.Errorf("CreateItem() expected error target %v, got: %v", tt.wantErrTarget, err)
				}

				return
			}

			if err != nil {
				t.Errorf("CreateItem() unexpected error: %v", err)
				return
			}

			tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatalf("failed to begin transaction: %v", err)
			}

			defer tx.Rollback()

			gotList, err := store.getList(ctx, tx, tt.item.ListID)
			if err != nil {
				t.Fatalf("failed to get list after create: %v", err)
			}

			if !gotList.Modified.After(listModified) {
				t.Errorf("expected parent list timestamp to bump, got old: %v, new: %v", listModified, gotList.Modified)
			}

			var count int
			countQuery := "SELECT COUNT(*) FROM items WHERE title = ?"
			wantTitle := strings.TrimSpace(tt.item.Title)
			err = db.QueryRow(countQuery, wantTitle).Scan(&count)
			if err != nil {
				t.Fatalf("failed to query item: %v", err)
			}

			if count != 1 {
				t.Errorf("expected 1 item with title %q, got %d", wantTitle, count)
			}

			var (
				gotItem  model.Item
				tagsJSON string
			)

			itemQuery := `
				SELECT
					id,
					list_id,
					project_id,
					title,
					COALESCE(description, ''),
					status,
					tags,
					project_tag,
					waiting_on
				FROM items
				WHERE title = ?
			`

			err = db.QueryRow(itemQuery, wantTitle).Scan(
				&gotItem.ID,
				&gotItem.ListID,
				&gotItem.ProjectID,
				&gotItem.Title,
				&gotItem.Description,
				&gotItem.Status,
				&tagsJSON,
				&gotItem.ProjectTag,
				&gotItem.WaitingOn,
			)
			if err != nil {
				t.Fatalf("failed to query item: %v", err)
			}

			if err := json.Unmarshal([]byte(tagsJSON), &gotItem.Tags); err != nil {
				t.Fatalf("failed to unmarshal tags: %v", err)
			}

			if gotItem.Tags == nil {
				gotItem.Tags = []string{}
			}

			opts := []cmp.Option{
				cmpopts.IgnoreFields(model.Item{}, "ID"),
			}

			if tt.item.ID != "" && gotItem.ID != tt.item.ID {
				t.Errorf("CreateItem() ID mismatch: want %q, got %q", tt.item.ID, gotItem.ID)
			} else if tt.item.ID == "" && gotItem.ID == "" {
				t.Errorf("CreateItem() failed to generate an ID for the item")
			}

			if diff := cmp.Diff(tt.wantItem, &gotItem, opts...); diff != "" {
				t.Errorf("CreateItem() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUpdateItem(t *testing.T) {
	t.Parallel()

	listModified := time.Date(2009, time.November, 10, 23, 0, 0, 0, time.UTC)

	tests := []struct {
		name          string
		setupDB       func(t *testing.T, db *sql.DB) string
		setupItem     func(id string) model.Item
		setupCtx      func() (context.Context, context.CancelFunc)
		wantItem      *model.Item
		wantErr       bool
		wantErrTarget error
	}{
		{
			name: "valid update (complex fields)",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Inbox", listModified,
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							title,
							description,
							list_id,
							waiting_on,
							tags,
							modified,
							created
						) VALUES (?, ?, ?, ?, ?, '[]', ?, ?)
					`, "item-1", "Original", "", "list-1", "", time.Now(), time.Now(),
				)

				return "item-1"
			},
			setupItem: func(id string) model.Item {
				item := model.Item{
					ID:          id,
					ExternalID:  new("ext-id"),
					ListID:      "list-1",
					Position:    99,
					Title:       "  Updated Title  ",
					Description: "  Line 1  \n    Line 2",
					Status:      model.StatusDone,
					Tags:        []string{"updated", "tag"},
					Modified:    time.Now(),
					Created:     time.Now(),
				}

				return item
			},
			wantItem: &model.Item{
				ExternalID:  new("ext-id"),
				ListID:      "list-1",
				Position:    0,
				Title:       "Updated Title",
				Description: "Line 1\n  Line 2",
				Status:      model.StatusDone,
				Tags:        []string{"updated", "tag"},
			},
		},
		{
			name: "valid project update (complex fields)",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", model.ListProjects, listModified,
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							title,
							description,
							list_id,
							waiting_on,
							tags,
							modified,
							created
						) VALUES (?, ?, ?, ?, ?, '[]', ?, ?)
					`, "item-1", "Original", "", "list-1", "", time.Now(), time.Now(),
				)

				return "item-1"
			},
			setupItem: func(id string) model.Item {
				item := model.Item{
					ID:          id,
					ExternalID:  new("ext-id"),
					ListID:      "list-1",
					Position:    99,
					Title:       "  Updated Title  ",
					Description: "  Line 1  \n    Line 2",
					ProjectTag:  new("proj-1"),
					Status:      model.StatusDone,
					Tags:        []string{"updated", "tag"},
					Modified:    time.Now(),
					Created:     time.Now(),
				}

				return item
			},
			wantItem: &model.Item{
				ExternalID:  new("ext-id"),
				ListID:      "list-1",
				Position:    0,
				Title:       "Updated Title",
				Description: "Line 1\n  Line 2",
				Status:      model.StatusDone,
				ProjectTag:  new("proj-1"),
				Tags:        []string{"updated", "tag"},
			},
		},
		{
			name: "resolve project id by external project id",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Inbox", listModified,
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							list_id,
							title,
							description,
							waiting_on,
							modified,
							created,
							external_id
						) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
					`, "existing-project-id", "list-1", "My Project", "", "", time.Now(), time.Now(), "ext-proj-1",
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							list_id,
							position,
							title,
							description,
							waiting_on,
							modified,
							created
						) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
					`, "item-1", "list-1", 1, "Original", "", "", time.Now(), time.Now(),
				)

				return "item-1"
			},
			setupItem: func(id string) model.Item {
				item := model.Item{
					ID:                id,
					ExternalID:        new("ext-id"),
					ListID:            "list-1",
					Position:          1,
					Title:             "Task for Project",
					ExternalProjectID: new("ext-proj-1"),
					Status:            model.StatusNotStarted,
					Tags:              []string{},
					Modified:          time.Now(),
					Created:           time.Now(),
				}

				return item
			},
			wantItem: &model.Item{
				ExternalID: new("ext-id"),
				ListID:     "list-1",
				Position:   1,
				Title:      "Task for Project",
				Status:     model.StatusNotStarted,
				ProjectID:  new("existing-project-id"),
				Tags:       []string{},
			},
		},
		{
			name: "resolve project id by project tag",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Inbox", listModified,
				)

				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-projects", model.ListProjects, listModified,
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							list_id,
							title,
							description,
							waiting_on,
							project_tag,
							modified,
							created
						) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
					`, "existing-project-id", "list-projects", "My Project", "", "", "proj-1", time.Now(), time.Now(),
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							list_id,
							position,
							title,
							description,
							waiting_on,
							modified,
							created
						) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
					`, "item-1", "list-1", 1, "Original", "", "", time.Now(), time.Now(),
				)

				return "item-1"
			},
			setupItem: func(id string) model.Item {
				item := model.Item{
					ID:         id,
					ExternalID: new("ext-id"),
					ListID:     "list-1",
					Position:   1,
					Title:      "Task for Project",
					ProjectTag: new("proj-1"),
					Status:     model.StatusNotStarted,
					Tags:       []string{},
					Modified:   time.Now(),
					Created:    time.Now(),
				}

				return item
			},
			wantItem: &model.Item{
				ExternalID: new("ext-id"),
				ListID:     "list-1",
				Position:   1,
				Title:      "Task for Project",
				Status:     model.StatusNotStarted,
				ProjectID:  new("existing-project-id"),
				Tags:       []string{},
			},
		},
		{
			name: "preserve project id when external id is nil and parent has no tag",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
                           INSERT INTO lists (id, name, modified)
                           VALUES (?, ?, ?)
                       `, "list-1", "Inbox", listModified,
				)

				mustExec(t, db,
					`
                           INSERT INTO lists (id, name, modified)
                           VALUES (?, ?, ?)
                       `, "list-projects", model.ListProjects, listModified,
				)

				mustExec(t, db,
					`
                           INSERT INTO items (
                               id,
                               list_id,
                               title,
                               description,
                               waiting_on,
                               modified,
                               created
                           ) VALUES (?, ?, ?, ?, ?, ?, ?)
                       `, "proj", "list-projects", "Project", "", "", time.Now(), time.Now(),
				)

				mustExec(t, db,
					`
                           INSERT INTO items (
                               id,
                               list_id,
                               project_id,
                               position,
                               title,
                               description,
                               waiting_on,
                               modified,
                               created
                           ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
                       `, "item-1", "list-1", "proj", 1, "Original", "", "", time.Now(), time.Now(),
				)

				return "item-1"
			},
			setupItem: func(id string) model.Item {
				item := model.Item{
					ID:       id,
					ListID:   "list-1",
					Position: 1,
					Title:    "Updated Task",
					Status:   model.StatusNotStarted,
					Modified: time.Now(),
					Created:  time.Now(),
				}

				return item
			},
			wantItem: &model.Item{
				ListID:    "list-1",
				Position:  1,
				Title:     "Updated Task",
				Status:    model.StatusNotStarted,
				ProjectID: new("proj"),
				Tags:      []string{},
			},
		},
		{
			name: "error resolving project id by missing external project id",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Inbox", listModified,
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							list_id,
							position,
							title,
							description,
							waiting_on,
							modified,
							created
						) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
					`, "item-1", "list-1", 1, "Original", "", "", time.Now(), time.Now(),
				)

				return "item-1"
			},
			setupItem: func(id string) model.Item {
				item := model.Item{
					ID:                id,
					ExternalID:        new("ext-id"),
					ListID:            "list-1",
					Title:             "Task for Missing Project",
					ExternalProjectID: new("does-not-exist"),
					Modified:          time.Now(),
				}

				return item
			},
			wantItem: &model.Item{
				ExternalID: new("ext-id"),
				ListID:     "list-1",
				Position:   1,
				Title:      "Task for Missing Project",
				Status:     model.StatusNotStarted,
				Tags:       []string{},
			},
		},
		{
			name: "ignore missing project tag on non-project list",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Inbox", listModified,
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							list_id,
							position,
							title,
							description,
							waiting_on,
							modified,
							created
						) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
					`, "item-1", "list-1", 1, "Original", "", "", time.Now(), time.Now(),
				)

				return "item-1"
			},
			setupItem: func(id string) model.Item {
				item := model.Item{
					ID:         id,
					ExternalID: new("ext-id"),
					ListID:     "list-1",
					Title:      "Task with Missing Tag",
					ProjectTag: new("missing-tag"),
					Modified:   time.Now(),
				}

				return item
			},
			wantItem: &model.Item{
				ExternalID: new("ext-id"),
				ListID:     "list-1",
				Position:   1,
				Title:      "Task with Missing Tag",
				Status:     model.StatusNotStarted,
				Tags:       []string{},
			},
		},
		{
			name: "update item fails when coalescing check finds no rows",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
                           INSERT INTO lists (id, name, modified)
                           VALUES (?, ?, ?)
                       `, "list-1", "Inbox", listModified,
				)

				return "non-existent-id"
			},
			setupItem: func(id string) model.Item {
				item := model.Item{
					ID:       id,
					ListID:   "list-1",
					Title:    "Valid Update",
					Modified: time.Now(),
				}

				return item
			},
			wantErr:       true,
			wantErrTarget: ErrNotFound,
		},
		{
			name: "do not coalesce project id when parent has a tag",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
                           INSERT INTO lists (id, name, modified)
                           VALUES (?, ?, ?)
                       `, "list-1", "Inbox", listModified,
				)

				mustExec(t, db,
					`
                           INSERT INTO lists (id, name, modified)
                           VALUES (?, ?, ?)
                       `, "list-projects", model.ListProjects, listModified,
				)

				mustExec(t, db,
					`
                           INSERT INTO items (
                               id,
                               list_id,
                               title,
                               description,
                               waiting_on,
                               project_tag,
                               modified,
                               created
                           ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                       `, "proj", "list-projects", "Project", "", "", "proj-1", time.Now(), time.Now(),
				)

				mustExec(t, db,
					`
                           INSERT INTO items (
                               id,
                               list_id,
                               project_id,
                               position,
                               title,
                               description,
                               waiting_on,
                               modified,
                               created
                           ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
                       `, "item-1", "list-1", "proj", 1, "Original", "", "", time.Now(), time.Now(),
				)

				return "item-1"
			},
			setupItem: func(id string) model.Item {
				item := model.Item{
					ID:       id,
					ListID:   "list-1",
					Position: 1,
					Title:    "Updated Task",
					Status:   model.StatusNotStarted,
					Modified: time.Now(),
					Created:  time.Now(),
				}

				return item
			},
			wantItem: &model.Item{
				ListID:   "list-1",
				Position: 1,
				Title:    "Updated Task",
				Status:   model.StatusNotStarted,
				Tags:     []string{},
			},
		},
		{
			name: "update item by external id only and break project link",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Inbox", listModified,
				)

				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-projects", model.ListProjects, listModified,
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							list_id,
							title,
							description,
							waiting_on,
							modified,
							created
						) VALUES (?, ?, ?, ?, ?, ?, ?)
					`, "tagless-proj", "list-projects", "Project", "", "", time.Now(), time.Now(),
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							title,
							description,
							list_id,
							project_id,
							waiting_on,
							tags,
							modified,
							created,
							external_id
						) VALUES (?, ?, ?, ?, ?, ?, '[]', ?, ?, ?)
					`, "item-1", "Original", "", "list-1", "tagless-proj", "", time.Now(), time.Now(), "ext-I1",
				)

				return "item-1"
			},
			setupItem: func(_ string) model.Item {
				item := model.Item{
					ExternalID: new("ext-I1"),
					ListID:     "list-1",
					Title:      "Updated Title",
					Status:     model.StatusNotStarted,
					Modified:   time.Now(),
				}

				return item
			},
			wantItem: &model.Item{
				ListID:     "list-1",
				Title:      "Updated Title",
				Status:     model.StatusNotStarted,
				Tags:       []string{},
				ExternalID: new("ext-I1"),
			},
		},
		{
			name: "update item resolving list id by external id",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified, external_id)
						VALUES (?, ?, ?, ?)
					`, "list-1", "Inbox", listModified, "external-list-1",
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							title,
							description,
							list_id,
							waiting_on,
							tags,
							modified,
							created
						) VALUES (?, ?, ?, ?, ?, '[]', ?, ?)
					`, "item-1", "Original Title", "", "list-1", "", time.Now(), time.Now(),
				)

				return "item-1"
			},
			setupItem: func(id string) model.Item {
				item := model.Item{
					ID:             id,
					ExternalID:     new("ext-id"),
					ListID:         "",
					ExternalListID: new("external-list-1"),
					Title:          "Updated Title",
					Modified:       time.Now(),
				}

				return item
			},
			wantItem: &model.Item{
				ExternalID: new("ext-id"),
				ID:         "item-1",
				ListID:     "list-1",
				Title:      "Updated Title",
				Status:     model.StatusNotStarted,
				Tags:       []string{},
			},
		},
		{
			name: "preserve item external id when nil",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Inbox", listModified,
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							title,
							description,
							list_id,
							waiting_on,
							tags,
							modified,
							created,
							external_id
						) VALUES (?, ?, ?, ?, ?, '[]', ?, ?, ?)
					`, "item-1", "Original", "", "list-1", "", time.Now(), time.Now(), "ext-I1",
				)

				return "item-1"
			},
			setupItem: func(id string) model.Item {
				item := model.Item{
					ID:                id,
					ListID:            "list-1",
					Title:             "Updated",
					Status:            model.StatusNotStarted,
					ExternalProjectID: new("dummy-ext-proj-id"),
					ExternalID:        nil,
					Modified:          time.Now(),
				}

				return item
			},
			wantItem: &model.Item{
				ListID:     "list-1",
				Title:      "Updated",
				Status:     model.StatusNotStarted,
				Tags:       []string{},
				ExternalID: new("ext-I1"),
			},
		},
		{
			name: "update item missing identifiers",
			setupDB: func(_ *testing.T, _ *sql.DB) string {
				return ""
			},
			setupItem: func(_ string) model.Item {
				item := model.Item{
					Title:    "Headless Item",
					ListID:   "list-1",
					Modified: time.Now(),
				}

				return item
			},
			wantErr: true,
		},
		{
			name: "update item with nonexistent external id",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Inbox", listModified,
				)

				return ""
			},
			setupItem: func(_ string) model.Item {
				item := model.Item{
					ExternalID: new("non-existent-ext"),
					Title:      "Headless Update",
					ListID:     "list-1",
					Modified:   time.Now(),
				}

				return item
			},
			wantErr:       true,
			wantErrTarget: ErrNotFound,
		},
		{
			name: "invalid item (validation failed)",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Inbox", listModified,
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							title,
							description,
							list_id,
							waiting_on,
							tags,
							modified,
							created
						) VALUES (?, ?, ?, ?, ?, '[]', ?, ?)
					`, "item-1", "Valid", "", "list-1", "", time.Now(), time.Now(),
				)

				return "item-1"
			},
			setupItem: func(id string) model.Item {
				item := model.Item{
					ID:         id,
					ExternalID: new("ext-id"),
					ListID:     "list-1",
					Title:      "",
					Modified:   time.Now(),
				}

				return item
			},
			wantErr: true,
		},
		{
			name: "nonexistent id",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Inbox", listModified,
				)

				return "non-existent"
			},
			setupItem: func(id string) model.Item {
				item := model.Item{
					ID:         id,
					ExternalID: new("ext-id"),
					ListID:     "list-1",
					Title:      "Valid",
					Modified:   time.Now(),
				}

				return item
			},
			wantErr:       true,
			wantErrTarget: ErrNotFound,
		},
		{
			name: "update item fails when resolving non-existent external list id",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Inbox", listModified,
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							title,
							description,
							list_id,
							waiting_on,
							tags,
							modified,
							created
						) VALUES (?, ?, ?, ?, ?, '[]', ?, ?)
					`, "item-1", "Original Title", "", "list-1", "", time.Now(), time.Now(),
				)

				return "item-1"
			},
			setupItem: func(id string) model.Item {
				item := model.Item{
					ID:             id,
					ExternalID:     new("ext-id"),
					ListID:         "",
					ExternalListID: new("fake-external-list"),
					Title:          "Updated Title",
					Modified:       time.Now(),
				}

				return item
			},
			wantErr:       true,
			wantErrTarget: ErrNotFound,
		},
		{
			name: "context cancellation",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Inbox", listModified,
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							title,
							description,
							list_id,
							waiting_on,
							tags,
							modified,
							created
						) VALUES (?, ?, ?, ?, ?, '[]', ?, ?)
					`, "item-1", "Valid", "", "list-1", "", time.Now(), time.Now(),
				)

				return "item-1"
			},
			setupItem: func(id string) model.Item {
				item := model.Item{
					ID:         id,
					ExternalID: new("ext-id"),
					ListID:     "list-1",
					Title:      "Valid",
					Modified:   time.Now(),
				}

				return item
			},
			setupCtx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()

				return ctx, cancel
			},
			wantErr: true,
		},
		{
			name: "list iteration fails due to corrupted sibling item tags",
			setupDB: func(t *testing.T, db *sql.DB) string {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Inbox", listModified,
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							title,
							description,
							list_id,
							waiting_on,
							tags,
							modified,
							created
						) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
					`, "item-to-update", "Valid Item", "", "list-1", "", "[]", time.Now(), time.Now(),
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							title,
							description,
							list_id,
							waiting_on,
							tags,
							modified,
							created
						) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
					`, "poisoned-item", "Poisoned Item", "", "list-1", "", "{bad-json", time.Now(), time.Now(),
				)

				return "item-to-update"
			},
			setupItem: func(id string) model.Item {
				item := model.Item{
					ID:         id,
					ExternalID: new("ext-id"),
					ListID:     "list-1",
					Title:      "Updated Valid Item",
					Modified:   time.Now(),
				}

				return item
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var (
				ctx    context.Context
				cancel context.CancelFunc
			)

			if tt.setupCtx != nil {
				ctx, cancel = tt.setupCtx()
				defer cancel()
			} else {
				ctx = t.Context()
			}

			tmpDir := t.TempDir()
			dbPath := filepath.Join(tmpDir, "test.db")
			logger := slog.New(slog.DiscardHandler)
			store, err := NewStore(t.Context(), dbPath, logger)
			if err != nil {
				t.Fatalf("failed to create store: %v", err)
			}

			db, err := sql.Open("sqlite3", dbPath)
			if err != nil {
				t.Fatalf("failed to open db for setup: %v", err)
			}

			defer db.Close()

			id := tt.setupDB(t, db)
			item := tt.setupItem(id)
			err = store.UpdateItem(ctx, &item)

			if tt.wantErr {
				if err == nil {
					t.Error("UpdateItem() expected error, got nil")
				} else if tt.wantErrTarget != nil && !errors.Is(err, tt.wantErrTarget) {
					t.Errorf("UpdateItem() expected error target %v, got: %v", tt.wantErrTarget, err)
				}

				return
			}

			if err != nil {
				t.Errorf("UpdateItem() unexpected error: %v", err)
				return
			}

			tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatalf("failed to begin transaction: %v", err)
			}

			defer tx.Rollback()

			gotList, err := store.getList(ctx, tx, item.ListID)
			if err != nil {
				t.Fatalf("failed to get list after update: %v", err)
			}

			if !gotList.Modified.After(listModified) {
				t.Errorf("expected parent list timestamp to bump, got old: %v, new: %v", listModified, gotList.Modified)
			}

			var (
				gotItem  model.Item
				tagsJSON string
			)

			query := `
				SELECT
					id,
					list_id,
					project_id,
					title,
					COALESCE(description, ''),
					status,
					tags,
					position,
					external_id,
					project_tag
				FROM items
				WHERE id = ?
			`

			err = db.QueryRow(query, id).Scan(
				&gotItem.ID,
				&gotItem.ListID,
				&gotItem.ProjectID,
				&gotItem.Title,
				&gotItem.Description,
				&gotItem.Status,
				&tagsJSON,
				&gotItem.Position,
				&gotItem.ExternalID,
				&gotItem.ProjectTag,
			)
			if err != nil {
				t.Fatalf("failed to query item: %v", err)
			}

			if err := json.Unmarshal([]byte(tagsJSON), &gotItem.Tags); err != nil {
				t.Fatalf("failed to unmarshal tags: %v", err)
			}

			if gotItem.Tags == nil {
				gotItem.Tags = []string{}
			}

			tt.wantItem.ID = id

			if diff := cmp.Diff(tt.wantItem, &gotItem); diff != "" {
				t.Errorf("UpdateItem() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDeleteItem(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		setupDB       func(t *testing.T, db *sql.DB) model.Item
		setupCtx      func() (context.Context, context.CancelFunc)
		wantErr       bool
		wantErrTarget error
	}{
		{
			name: "valid delete",
			setupDB: func(t *testing.T, db *sql.DB) model.Item {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Inbox", time.Now(),
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							title,
							description,
							list_id,
							waiting_on,
							tags,
							modified,
							created
						) VALUES (?, ?, ?, ?, ?, '[]', ?, ?)
					`, "item-1", "Item to Delete", "", "list-1", "", time.Now(), time.Now(),
				)

				item := model.Item{
					ID: "item-1",
				}

				return item
			},
		},
		{
			name: "delete item by external id",
			setupDB: func(t *testing.T, db *sql.DB) model.Item {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Inbox", time.Now(),
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							title,
							description,
							list_id,
							waiting_on,
							tags,
							modified,
							created,
							external_id
						) VALUES (?, ?, ?, ?, ?, '[]', ?, ?, ?)
					`, "item-1", "Item to Delete", "", "list-1", "", time.Now(), time.Now(), "ext-I1",
				)

				item := model.Item{
					ExternalID: new("ext-I1"),
				}

				return item
			},
		},
		{
			name: "delete item missing identifiers",
			setupDB: func(_ *testing.T, _ *sql.DB) model.Item {
				item := model.Item{
					Title: "Headless Item",
				}

				return item
			},
			wantErr: true,
		},
		{
			name: "delete item with nonexistent external id",
			setupDB: func(_ *testing.T, _ *sql.DB) model.Item {
				item := model.Item{
					ExternalID: new("non-existent-ext"),
				}

				return item
			},
			wantErr:       true,
			wantErrTarget: ErrNotFound,
		},
		{
			name: "nonexistent id",
			setupDB: func(_ *testing.T, _ *sql.DB) model.Item {
				item := model.Item{
					ID: "non-existent",
				}

				return item
			},
			wantErr:       true,
			wantErrTarget: ErrNotFound,
		},
		{
			name: "context cancellation",
			setupDB: func(t *testing.T, db *sql.DB) model.Item {
				mustExec(t, db,
					`
						INSERT INTO lists (id, name, modified)
						VALUES (?, ?, ?)
					`, "list-1", "Inbox", time.Now(),
				)

				mustExec(t, db,
					`
						INSERT INTO items (
							id,
							title,
							description,
							list_id,
							waiting_on,
							tags,
							modified,
							created
						) VALUES (?, ?, ?, ?, ?, '[]', ?, ?)
					`, "item-1", "Valid", "", "list-1", "", time.Now(), time.Now(),
				)

				item := model.Item{
					ID: "item-1",
				}

				return item
			},
			setupCtx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()

				return ctx, cancel
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var (
				ctx    context.Context
				cancel context.CancelFunc
			)

			if tt.setupCtx != nil {
				ctx, cancel = tt.setupCtx()
				defer cancel()
			} else {
				ctx = t.Context()
			}

			tmpDir := t.TempDir()
			dbPath := filepath.Join(tmpDir, "test.db")
			logger := slog.New(slog.DiscardHandler)
			store, err := NewStore(t.Context(), dbPath, logger)
			if err != nil {
				t.Fatalf("failed to create store: %v", err)
			}

			db, err := sql.Open("sqlite3", dbPath)
			if err != nil {
				t.Fatalf("failed to open db for setup: %v", err)
			}

			defer db.Close()

			item := tt.setupDB(t, db)
			err = store.DeleteItem(ctx, &item)

			if tt.wantErr {
				if err == nil {
					t.Error("DeleteItem() expected error, got nil")
				} else if tt.wantErrTarget != nil && !errors.Is(err, tt.wantErrTarget) {
					t.Errorf("DeleteItem() expected error target %v, got: %v", tt.wantErrTarget, err)
				}

				return
			}

			if err != nil {
				t.Errorf("DeleteItem() unexpected error: %v", err)
				return
			}

			tx, _ := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			items, err := store.listAllItems(ctx, tx)
			tx.Rollback()
			if err != nil {
				t.Fatalf("failed to get all items: %v", err)
			}

			if diff := cmp.Diff([]*model.Item(nil), items); diff != "" {
				t.Errorf("DeleteItem() items mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// mustExec is a test helper that executes a query and fails the test if it returns an error.
func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("failed to execute query: %v", err)
	}
}

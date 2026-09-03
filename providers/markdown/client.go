// Package markdown implements the markdown file provider.
package markdown

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/danrneal/gtd-cli/model"
)

// Client is a markdown file provider client.
type Client struct {
	filepath    string
	lastModTime time.Time
	mu          sync.RWMutex
	logger      *slog.Logger
}

// NewClient creates a new markdown client with the given file path.
func NewClient(filepath string, logger *slog.Logger) *Client {
	client := &Client{
		filepath: filepath,
		logger:   logger,
	}

	if stat, err := os.Stat(filepath); err == nil {
		client.lastModTime = stat.ModTime()
	}

	return client
}

// GetKey extracts the ID from the resource.
func (c *Client) GetKey(resource model.Resource) string {
	if id := resource.GetID(); id != "" {
		return id
	}

	return ""
}

// CreateList creates a new list in the markdown file.
func (c *Client) CreateList(ctx context.Context, list *model.List) error {
	list.Clean()
	if err := list.Validate(); err != nil {
		return fmt.Errorf("invalid list: %w", err)
	}

	if list.Status == model.StatusDeleted {
		return errors.New("cannot create a list with status 'deleted'")
	}

	lists, err := c.readFile()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	list.Position = min(list.Position, len(lists))

	newList := *list
	newList.Items = nil

	lists = slices.Insert(lists, newList.Position, newList)

	c.logger.InfoContext(ctx, "Markdown: Creating list", "id", list.ID, "name", list.Name)
	err = c.writeFile(lists)

	return err
}

// ListLists retrieves all lists from the markdown file.
func (c *Client) ListLists(_ context.Context) ([]model.List, error) {
	lists, err := c.readFile()
	return lists, err
}

// UpdateList updates an existing list and its items in the markdown file.
func (c *Client) UpdateList(_ context.Context, list, currentList *model.List) error {
	list.Clean()
	if err := list.Validate(); err != nil {
		return fmt.Errorf("invalid list: %w", err)
	}

	if list.ID == "" {
		return errors.New("failed to update list: missing ID")
	}

	lists, err := c.readFile()
	if err != nil {
		return err
	}

	idx := slices.IndexFunc(lists, func(l model.List) bool {
		return l.ID == list.ID || l.Equivalent(list)
	})

	if idx == -1 {
		return fmt.Errorf("failed to update list: list %q not found", list.ID)
	}

	lists[idx].ID = list.ID
	lists[idx].Name = list.Name

	if idx != list.Position {
		if list.Position >= len(lists) {
			list.Position = len(lists) - 1
		}

		listToMove := lists[idx]
		lists = slices.Delete(lists, idx, idx+1)
		lists = slices.Insert(lists, list.Position, listToMove)

		idx = list.Position
	}

	itemsToMove := calculateItemsToMove(list, currentList.Items)
	for _, item := range itemsToMove {
		err = c.moveItem(lists, item, idx)
		if err != nil {
			return err
		}
	}

	err = c.writeFile(lists)

	return err
}

// DeleteList removes a list from the markdown file.
func (c *Client) DeleteList(ctx context.Context, list *model.List) error {
	if list.ID == "" {
		return errors.New("failed to delete list: missing ID")
	}

	lists, err := c.readFile()
	if err != nil {
		return err
	}

	idx := slices.IndexFunc(lists, func(l model.List) bool {
		return l.ID == list.ID
	})

	if idx == -1 {
		return fmt.Errorf("failed to delete list: list %q not found", list.ID)
	}

	lists = slices.Delete(lists, idx, idx+1)

	c.logger.InfoContext(ctx, "Markdown: Deleting list", "id", list.ID, "name", list.Name)
	err = c.writeFile(lists)

	return err
}

// CreateItem adds a new item to a list in the markdown file.
func (c *Client) CreateItem(ctx context.Context, item *model.Item, previousItemID string) error {
	item.Clean()
	if err := item.Validate(); err != nil {
		return fmt.Errorf("invalid item: %w", err)
	}

	if item.Status == model.StatusDeleted {
		return errors.New("cannot create an item with status 'deleted'")
	}

	if item.ListID == "" {
		return errors.New("failed to create item: missing list ID")
	}

	lists, err := c.readFile()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	listIdx := slices.IndexFunc(lists, func(l model.List) bool {
		return l.ID == item.ListID
	})

	if listIdx == -1 {
		return fmt.Errorf("failed to create item: list %q not found", item.ListID)
	}

	list := lists[listIdx]

	prevItemIdx := slices.IndexFunc(list.Items, func(i *model.Item) bool {
		return previousItemID != "" && i.ID == previousItemID
	})

	if prevItemIdx == -1 && previousItemID != "" {
		return fmt.Errorf("state corruption: anchor item %q not found in markdown", previousItemID)
	}

	list.Items = slices.Insert(list.Items, prevItemIdx+1, item)
	lists[listIdx] = list

	c.logger.InfoContext(ctx, "Markdown: Creating item", "id", item.ID, "title", item.Title, "listId", item.ListID)
	err = c.writeFile(lists)

	return err
}

// UpdateItem updates an existing item in the markdown file.
func (c *Client) UpdateItem(ctx context.Context, item *model.Item) error {
	item.Clean()
	if err := item.Validate(); err != nil {
		return fmt.Errorf("invalid item: %w", err)
	}

	if item.ID == "" || item.ListID == "" {
		return errors.New("failed to update item: missing identifiers")
	}

	lists, err := c.readFile()
	if err != nil {
		return err
	}

	listIdx := slices.IndexFunc(lists, func(l model.List) bool {
		return l.ID == item.ListID
	})

	if listIdx == -1 {
		return fmt.Errorf("failed to update item: list %q not found", item.ListID)
	}

	list := lists[listIdx]

	itemIdx := slices.IndexFunc(list.Items, func(i *model.Item) bool {
		return i.ID == item.ID || i.Equivalent(item)
	})

	if itemIdx == -1 {
		return fmt.Errorf("failed to update item: item %q not found", item.ID)
	}

	list.Items[itemIdx] = item

	c.logger.InfoContext(ctx, "Markdown: Updating item", "id", item.ID, "title", item.Title, "listId", item.ListID)
	err = c.writeFile(lists)

	return err
}

// moveItem safely extracts an item from its source list (if it exists)
// and inserts it into the specified destination list at the correct position.
func (c *Client) moveItem(lists []model.List, item *model.Item, destinationListIdx int) error {
	sourceListIdx := slices.IndexFunc(lists, func(l model.List) bool {
		return l.ID == item.ListID
	})

	if sourceListIdx == -1 {
		return fmt.Errorf("source list %q not found for move", item.ListID)
	}

	sourceList := lists[sourceListIdx]
	itemIdx := slices.IndexFunc(sourceList.Items, func(i *model.Item) bool {
		return i.ID == item.ID
	})

	if itemIdx == -1 {
		return fmt.Errorf("item %q not found in source list for move", item.ID)
	}

	sourceList.Items = slices.Delete(sourceList.Items, itemIdx, itemIdx+1)
	lists[sourceListIdx] = sourceList

	destinationList := lists[destinationListIdx]
	destinationList.Items = slices.Insert(destinationList.Items, item.Position, item)
	lists[destinationListIdx] = destinationList

	return nil
}

// DeleteItem removes an item from the markdown file.
func (c *Client) DeleteItem(ctx context.Context, item *model.Item) error {
	if item.ID == "" || item.ListID == "" {
		return errors.New("failed to delete item: missing identifiers")
	}

	lists, err := c.readFile()
	if err != nil {
		return err
	}

	listIdx := slices.IndexFunc(lists, func(l model.List) bool {
		return l.ID == item.ListID
	})

	if listIdx == -1 {
		return fmt.Errorf("failed to delete item: list %q not found", item.ListID)
	}

	list := lists[listIdx]

	itemIdx := slices.IndexFunc(list.Items, func(i *model.Item) bool {
		return i.ID == item.ID
	})

	if itemIdx == -1 {
		return fmt.Errorf("failed to delete item: item %q not found", item.ID)
	}

	list.Items = slices.Delete(list.Items, itemIdx, itemIdx+1)
	lists[listIdx] = list

	c.logger.InfoContext(ctx, "Markdown: Deleting item", "id", item.ID, "title", item.Title, "listId", item.ListID)
	err = c.writeFile(lists)

	return err
}

// readFile opens the markdown file, reads its modification time, and parses its contents.
// If the file does not exist, it safely returns an empty slice to allow for bootstrapping.
func (c *Client) readFile() ([]model.List, error) {
	stat, err := os.Stat(c.filepath)
	if err != nil {
		return nil, fmt.Errorf("failed to access markdown file: %w", err)
	}

	fileBytes, err := os.ReadFile(c.filepath)
	if err != nil {
		return nil, fmt.Errorf("failed to read markdown file: %w", err)
	}

	if len(fileBytes) == 0 {
		return nil, fs.ErrNotExist
	}

	reader := bytes.NewReader(fileBytes)
	lists, err := parse(reader, stat.ModTime())
	if err != nil {
		return nil, fmt.Errorf("failed to parse markdown file: %w", err)
	}

	var renderedBuf bytes.Buffer
	if err := render(&renderedBuf, lists); err != nil {
		return nil, fmt.Errorf("failed to render markdown for comparison: %w", err)
	}

	if !bytes.Equal(fileBytes, renderedBuf.Bytes()) {
		if err := c.writeFile(lists, withModTime(stat.ModTime())); err != nil {
			return nil, err
		}
	}

	return lists, nil
}

type writeOption func(*writeOptions)

type writeOptions struct {
	modTime *time.Time
}

func withModTime(t time.Time) writeOption {
	modTimeOpt := func(o *writeOptions) {
		o.modTime = &t
	}

	return modTimeOpt
}

// writeFile renders the provided lists to Markdown and atomically overwrites the file.
func (c *Client) writeFile(lists []model.List, opts ...writeOption) error {
	var options writeOptions
	for _, opt := range opts {
		opt(&options)
	}

	var buf bytes.Buffer
	if err := render(&buf, lists); err != nil {
		return fmt.Errorf("failed to render markdown file: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	file, err := os.OpenFile(c.filepath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("failed to open markdown file for writing: %w", err)
	}

	defer file.Close()

	if _, err = file.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("failed to write to markdown file: %w", err)
	}

	if err = file.Close(); err != nil {
		return fmt.Errorf("failed to close markdown file: %w", err)
	}

	if options.modTime != nil {
		if err := os.Chtimes(c.filepath, *options.modTime, *options.modTime); err != nil {
			return fmt.Errorf("failed to restore original modification time: %w", err)
		}
	} else {
		stat, err := os.Stat(c.filepath)
		if err != nil {
			return fmt.Errorf("failed to stat markdown file after writing: %w", err)
		}

		c.lastModTime = stat.ModTime()
	}

	return nil
}

// calculateItemsToMove determines which items have been relocated or reordered within
// a list and need their positions updated.
func calculateItemsToMove(list *model.List, currentItems []*model.Item) []*model.Item {
	var itemsToMove []*model.Item
	for i, item := range list.Items {
		if i < len(currentItems) && item.ID == currentItems[i].ID {
			continue
		}

		itemsToMove = append(itemsToMove, item)
	}

	return itemsToMove
}

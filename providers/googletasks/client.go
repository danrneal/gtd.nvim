// Package googletasks implements the Google Tasks provider.
package googletasks

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/api/tasks/v1"

	"github.com/danrneal/gtd-cli/model"
	"github.com/danrneal/gtd-cli/providers/util/reorder"
)

const (
	statusNeedsAction = "needsAction" // Google Tasks API representation of an incomplete task
	statusCompleted   = "completed"   // Google Tasks API representation of a completed task
	maxTaskResults    = 100           // Maximum number of tasks to retrieve per API request
)

// Client is a wrapper around the Google Tasks service.
type Client struct {
	service      *tasks.Service
	pollInterval time.Duration
	logger       *slog.Logger
}

// NewClient creates a new Google Tasks client.
func NewClient(service *tasks.Service, pollInterval time.Duration, logger *slog.Logger) *Client {
	client := &Client{
		service:      service,
		pollInterval: pollInterval,
		logger:       logger,
	}

	return client
}

// GetKey extracts the external ID from the resource.
func (c *Client) GetKey(resource model.Resource) string {
	if extID := resource.GetExternalID(); extID != nil {
		return *extID
	}

	return ""
}

// CreateList creates a new task list on the Google Tasks service.
func (c *Client) CreateList(ctx context.Context, list *model.List) error {
	list.Clean()
	if err := list.Validate(); err != nil {
		return fmt.Errorf("invalid list: %w", err)
	}

	if list.Status == model.StatusDeleted {
		return errors.New("cannot create a list with status 'deleted'")
	}

	tasklist := &tasks.TaskList{
		Title: list.Name,
	}

	c.logger.InfoContext(ctx, "Google Tasks: Inserting tasklist", "title", tasklist.Title)
	taskList, err := c.service.Tasklists.Insert(tasklist).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("failed to create tasklist %s: %w", tasklist.Title, err)
	}

	list.ExternalID = &taskList.Id

	return nil
}

// ListLists retrieves all task lists from Google Tasks.
func (c *Client) ListLists(ctx context.Context) ([]model.List, error) {
	resp, err := c.service.Tasklists.List().Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve tasklists: %w", err)
	}

	g, ctx := errgroup.WithContext(ctx)

	lists := make([]model.List, len(resp.Items))
	for i, tasklist := range resp.Items {
		list := model.List{
			Name:       tasklist.Title,
			Position:   i,
			Status:     model.StatusOpen,
			ExternalID: &tasklist.Id,
		}

		if tasklist.Updated != "" {
			if updated, err := time.Parse(time.RFC3339, tasklist.Updated); err == nil {
				list.Modified = updated
			}
		}

		g.Go(func() error {
			items, err := c.listItems(ctx, &list)
			if err != nil {
				return err
			}

			list.Items = items
			lists[i] = list

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("failed to retrieve items for all tasklists: %w", err)
	}

	return lists, nil
}

// UpdateList updates an existing task list on Google Tasks.
func (c *Client) UpdateList(ctx context.Context, list, currentList *model.List) error {
	list.Clean()
	if err := list.Validate(); err != nil {
		return fmt.Errorf("invalid list: %w", err)
	}

	if list.ExternalID == nil {
		return errors.New("failed to update list: missing external ID")
	}

	if list.Name != currentList.Name {
		tasklist := &tasks.TaskList{
			Title: list.Name,
		}

		c.logger.InfoContext(
			ctx,
			"Google Tasks: Patching tasklist",
			"title",
			tasklist.Title,
			"externalId",
			*list.ExternalID,
		)

		if _, err := c.service.Tasklists.Patch(*list.ExternalID, tasklist).Context(ctx).Do(); err != nil {
			return fmt.Errorf("failed to update tasklist %s: %w", tasklist.Title, err)
		}
	}

	updateList := *list
	updateList.Items = make([]*model.Item, 0, len(list.Items))
	for _, item := range list.Items {
		hasNewDestList := item.ExternalListID != nil && *item.ExternalListID != *list.ExternalID
		if item.Status != model.StatusDone || hasNewDestList {
			updateList.Items = append(updateList.Items, item)
		}
	}

	moves := reorder.CalculateMoves(&updateList, currentList.Items)
	for _, move := range moves {
		if err := c.moveItem(ctx, move); err != nil {
			return err
		}
	}

	return nil
}

// DeleteList deletes a task list from Google Tasks.
func (c *Client) DeleteList(ctx context.Context, list *model.List) error {
	if list.ExternalID == nil {
		return errors.New("failed to delete list: missing external ID")
	}

	c.logger.InfoContext(ctx, "Google Tasks: Deleting tasklist", "externalId", *list.ExternalID)
	if err := c.service.Tasklists.Delete(*list.ExternalID).Context(ctx).Do(); err != nil {
		return fmt.Errorf("failed to delete tasklist: %w", err)
	}

	return nil
}

// CreateItem creates a new task in the specified Google Task list.
// If previousItemID is provided, the task is inserted after that item.
// It renders the item's title to include metadata (project, tags, due date) compatible with the parser.
func (c *Client) CreateItem(ctx context.Context, item *model.Item, previousItemID string) error {
	item.Clean()
	if err := item.Validate(); err != nil {
		return fmt.Errorf("invalid item: %w", err)
	}

	if item.Status == model.StatusDeleted {
		return errors.New("cannot create an item with status 'deleted'")
	}

	if item.ExternalListID == nil {
		return errors.New("failed to create item: missing external list ID")
	}

	title := renderTitle(item)
	status := statusNeedsAction
	if item.Status == model.StatusDone {
		status = statusCompleted
	}

	var due string
	if item.Snoozed != nil {
		due = item.Snoozed.Format(time.RFC3339)
	}

	task := &tasks.Task{
		Title:  title,
		Notes:  item.Description,
		Status: status,
		Due:    due,
	}

	tasksInsertCall := c.service.Tasks.Insert(*item.ExternalListID, task)
	if previousItemID != "" {
		tasksInsertCall.Previous(previousItemID)
	}

	c.logger.InfoContext(
		ctx,
		"Google Tasks: Inserting task",
		"title",
		task.Title,
		"externalListId",
		*item.ExternalListID,
		"previousItemId",
		previousItemID,
	)

	task, err := tasksInsertCall.Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("failed to insert task: %w", err)
	}

	item.ExternalID = &task.Id

	return nil
}

// listItems retrieves all tasks from the specified list and converts them to internal Items.
// It handles fetching, sorting, and parsing metadata from task titles.
func (c *Client) listItems(ctx context.Context, list *model.List) ([]*model.Item, error) {
	resp, err := c.service.Tasks.List(*list.ExternalID).ShowHidden(true).MaxResults(maxTaskResults).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve tasks for list %q: %w", list.Name, err)
	}

	slices.SortFunc(resp.Items, func(a, b *tasks.Task) int {
		return cmp.Compare(a.Position, b.Position)
	})

	var items []*model.Item
	for i, task := range resp.Items {
		var item *model.Item
		if strings.HasPrefix(list.Name, model.ListWaitingFor) {
			item = parseWaitingForTitle(task.Title)
		} else {
			item = parseTitle(task.Title)
		}

		item.Position = i
		if task.Status == statusCompleted {
			item.Status = model.StatusDone
		} else {
			item.Status = model.StatusOpen
		}

		item.Description = task.Notes
		if task.Due != "" {
			if due, err := time.Parse(time.RFC3339, task.Due); err == nil {
				item.Snoozed = &due
			}
		}

		if task.Updated != "" {
			if updated, err := time.Parse(time.RFC3339, task.Updated); err == nil {
				item.Modified = updated
			}
		}

		externalID := task.Id
		item.ExternalID = &externalID
		item.ExternalListID = list.ExternalID
		items = append(items, item)
	}

	return items, nil
}

// UpdateItem updates an existing task in the specified Google Task list.
func (c *Client) UpdateItem(ctx context.Context, item *model.Item) error {
	item.Clean()
	if err := item.Validate(); err != nil {
		return fmt.Errorf("invalid item: %w", err)
	}

	if item.ExternalID == nil || item.ExternalListID == nil {
		return errors.New("failed to update item: missing external identifiers")
	}

	title := renderTitle(item)
	status := statusNeedsAction
	if item.Status == model.StatusDone {
		status = statusCompleted
	}

	task := &tasks.Task{
		Title:  title,
		Notes:  item.Description,
		Status: status,
	}

	if item.Description == "" {
		task.NullFields = append(task.NullFields, "Notes")
	}

	if item.Snoozed != nil {
		task.Due = item.Snoozed.Format(time.RFC3339)
	} else {
		task.NullFields = append(task.NullFields, "Due")
	}

	c.logger.InfoContext(
		ctx,
		"Google Tasks: Patching task",
		"title",
		task.Title,
		"externalId",
		*item.ExternalID,
		"externalListId",
		*item.ExternalListID,
	)

	if _, err := c.service.Tasks.Patch(*item.ExternalListID, *item.ExternalID, task).Context(ctx).Do(); err != nil {
		return fmt.Errorf("failed to update task: %w", err)
	}

	return nil
}

// moveItem moves a task to a new position, potentially in a different list.
func (c *Client) moveItem(ctx context.Context, move reorder.Move) error {
	tasksMoveCall := c.service.Tasks.Move(move.SourceListID, move.ItemID)
	if move.PreviousItemID != "" {
		tasksMoveCall.Previous(move.PreviousItemID)
	}

	if move.DestinationListID != "" {
		tasksMoveCall.DestinationTasklist(move.DestinationListID)
	}

	c.logger.InfoContext(
		ctx,
		"Google Tasks: Moving task",
		"itemId",
		move.ItemID,
		"sourceListId",
		move.SourceListID,
		"destinationListId",
		move.DestinationListID,
		"previousItemId",
		move.PreviousItemID,
	)

	if _, err := tasksMoveCall.Context(ctx).Do(); err != nil {
		return fmt.Errorf("failed to move task: %w", err)
	}

	return nil
}

// DeleteItem deletes a task from the specified Google Task list.
func (c *Client) DeleteItem(ctx context.Context, item *model.Item) error {
	if item.ExternalID == nil || item.ExternalListID == nil {
		return errors.New("failed to delete item: missing external identifiers")
	}

	c.logger.InfoContext(
		ctx,
		"Google Tasks: Deleting task",
		"title",
		item.Title,
		"externalId",
		*item.ExternalID,
		"externalListId",
		*item.ExternalListID,
	)

	if err := c.service.Tasks.Delete(*item.ExternalListID, *item.ExternalID).Context(ctx).Do(); err != nil {
		return fmt.Errorf("failed to delete task: %w", err)
	}

	return nil
}

// renderTitle constructs the task title by combining it with metadata (project, tags, due date, waiting on).
func renderTitle(item *model.Item) string {
	titleParts := []string{item.Title}
	if item.ProjectTag != nil {
		projectTag := fmt.Sprintf("+%s", *item.ProjectTag)
		titleParts = append(titleParts, projectTag)
	}

	if item.Due != nil {
		due := fmt.Sprintf("due:%s", item.Due.Format("2006-01-02"))
		titleParts = append(titleParts, due)
	}

	for _, tag := range item.Tags {
		tagStr := fmt.Sprintf("#%s", tag)
		titleParts = append(titleParts, tagStr)
	}

	title := strings.Join(titleParts, " ")

	if item.WaitingOn != "" {
		created := item.Created.Format("2006-01-02")
		title = fmt.Sprintf("%s - %s - %s", item.WaitingOn, title, created)
	}

	return title
}

// parseWaitingForTitle extracts the waiting-on person from the title and then
// delegates to parseTitle for the rest of the metadata.
func parseWaitingForTitle(title string) *model.Item {
	var waitingOn string
	parts := strings.Split(title, " - ")
	if len(parts) > 1 {
		waitingOn = strings.TrimSpace(parts[0])
		title = parts[1]
	}

	item := parseTitle(title)
	if waitingOn != "" {
		item.WaitingOn = waitingOn
	}

	if parts[len(parts)-1] != title {
		created := strings.TrimSpace(parts[len(parts)-1])
		if created, err := time.Parse("2006-01-02", created); err == nil {
			item.Created = created
		}
	}

	return item
}

// parseTitle extracts metadata (project, tags, due date) from the task title string.
func parseTitle(title string) *model.Item {
	item := &model.Item{}

	var titleParts []string
	titleFields := strings.FieldsSeq(title)
	for titleField := range titleFields {
		switch {
		case strings.HasPrefix(titleField, "+"):
			if len(titleField) > 1 {
				projectTag := titleField[1:]
				item.ProjectTag = &projectTag
			}
		case strings.HasPrefix(titleField, "due:"):
			due := strings.TrimPrefix(titleField, "due:")
			if due, err := time.Parse("2006-01-02", due); err == nil {
				item.Due = &due
			}
		case strings.HasPrefix(titleField, "#"):
			if len(titleField) > 1 {
				item.Tags = append(item.Tags, titleField[1:])
			}
		default:
			titleParts = append(titleParts, titleField)
		}
	}

	item.Title = strings.Join(titleParts, " ")

	return item
}

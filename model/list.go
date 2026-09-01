package model

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	// ListInbox is the reserved name for the "Inbox" list.
	ListInbox = "Inbox"
	// ListWaitingFor is the reserved name for the "Waiting For" list.
	ListWaitingFor = "Waiting For"
	// ListProjects is the reserved name for the "Projects" list.
	ListProjects = "Projects"
	// ListSnoozed is the reserved name for the "Snoozed" list.
	ListSnoozed = "Snoozed"
)

// List represents a named collection of tasks (Items).
type List struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Position   int       `json:"position"`
	Status     Status    `json:"status"`
	Modified   time.Time `json:"modified"`
	ExternalID *string   `json:"externalId"`
	Items      []*Item   `json:"items"`
}

// GetID returns the internal ID of the list.
func (l *List) GetID() string {
	return l.ID
}

// GetExternalID returns the external ID of the list.
func (l *List) GetExternalID() *string {
	return l.ExternalID
}

// Clean formats the list data into its canonical representation.
func (l *List) Clean() {
	l.Name = strings.TrimSpace(l.Name)
	if l.Status == "" {
		l.Status = StatusOpen
	}

	l.sortItems()
}

// Validate checks if the current state of the list satisfies domain invariants.
func (l *List) Validate() error {
	if l.Name == "" {
		return errors.New("list name cannot be empty")
	}

	switch l.Status {
	case StatusOpen, StatusDeleted:
		// Valid status
	default:
		return fmt.Errorf("invalid list status: %q", l.Status)
	}

	if l.Modified.IsZero() {
		return errors.New("list modified timestamp cannot be zero")
	}

	return nil
}

// Equivalent compares the content of two lists, ignoring metadata like Modified timestamps
// and dynamically applying nuanced checks for primary/external IDs.
func (l *List) Equivalent(other *List) bool {
	if other == nil {
		return false
	}

	if l.ID != "" && other.ID != "" && l.ID != other.ID {
		return false
	}

	if l.ExternalID != nil && other.ExternalID != nil && *l.ExternalID != *other.ExternalID {
		return false
	}

	if l.Name != other.Name {
		return false
	}

	if l.Status != other.Status {
		return false
	}

	items := slices.Clone(l.Items)
	items = slices.DeleteFunc(items, func(i *Item) bool {
		return i.Status == StatusDeleted
	})

	otherItems := slices.Clone(other.Items)
	otherItems = slices.DeleteFunc(otherItems, func(i *Item) bool {
		return i.Status == StatusDeleted
	})

	if len(items) != len(otherItems) {
		return false
	}

	for i, item := range items {
		otherItem := otherItems[i]

		if item.ID != "" && otherItem.ID != "" && item.ID != otherItem.ID {
			return false
		}

		if item.ExternalID != nil && otherItem.ExternalID != nil && *item.ExternalID != *otherItem.ExternalID {
			return false
		}
	}

	return true
}

// Contains evaluates if the provided item belongs to this list.
// It checks both the internal ID and the external provider ID to support cross-system matching.
func (l *List) Contains(item *Item) bool {
	if item.ListID != "" && item.ListID == l.ID {
		return true
	}

	if item.ExternalListID != nil && equalStringPtr(item.ExternalListID, l.ExternalID) {
		return true
	}

	return false
}

// sortItems dynamically orders the list's items according to domain rules.
// Default lists sort by Status (InProgress -> NotStarted -> Done).
// "Waiting For" lists sort by Created date, and "Snoozed" lists sort by Snoozed date.
func (l *List) sortItems() {
	sortFunc := compareItems
	if strings.HasPrefix(l.Name, ListWaitingFor) {
		sortFunc = compareWaitingForItems
	} else if strings.HasPrefix(l.Name, ListSnoozed) {
		sortFunc = compareSnoozedItems
	}

	slices.SortStableFunc(l.Items, func(a, b *Item) int {
		return sortFunc(a, b)
	})

	for i, item := range l.Items {
		item.Position = i
	}
}

// compareItems determines the sorting order of items based primarily on their Status.
// InProgress items appear first, followed by NotStarted items, and finally Done items.
func compareItems(a, b *Item) int {
	return statusRank(a.Status) - statusRank(b.Status)
}

// statusRank returns a numeric ranking for sorting by Status, prioritizing active work.
func statusRank(s Status) int {
	switch s {
	case StatusInProgress:
		return -1
	case StatusDone:
		return 1
	default:
		return 0
	}
}

// compareWaitingForItems determines the sorting order for items in a "Waiting For" list.
// It primarily groups items by their status, and secondarily sorts them by Creation date (newest first)
// so that the most recent waiting items are shown at the top.
func compareWaitingForItems(a, b *Item) int {
	if c := compareItems(a, b); c != 0 {
		return c
	}

	if a.WaitingOn == "" {
		return 0
	}

	if b.WaitingOn == "" {
		return -1
	}

	return b.Created.Compare(a.Created)
}

// compareSnoozedItems determines the sorting order for items in a "Snoozed" list.
// It primarily groups items by their status, and secondarily sorts them by Snoozed date
// so that items waking up soonest are shown at the top.
func compareSnoozedItems(a, b *Item) int {
	if c := compareItems(a, b); c != 0 {
		return c
	}

	if a.Snoozed == nil {
		return 0
	}

	if b.Snoozed == nil {
		return -1
	}

	return a.Snoozed.Compare(*b.Snoozed)
}

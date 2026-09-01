package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"

	"github.com/danrneal/gtd-cli/model"
)

// Syncer manages the synchronization of items and lists between a local provider and a remote provider.
type Syncer struct {
	local  Provider
	remote RemoteProvider
	getKey func(model.Resource) string
}

// NewSyncer creates a new Syncer instance with the given local and remote providers.
func NewSyncer(local Provider, remote RemoteProvider) *Syncer {
	syncer := &Syncer{
		local:  local,
		remote: remote,
		getKey: remote.GetKey,
	}

	return syncer
}

// Push synchronizes changes from the local provider to the remote provider.
// It returns true if any changes were pushed.
func (s *Syncer) Push(ctx context.Context, syncStart time.Time) error {
	_, err := s.oneWaySync(ctx, s.local, s.remote, syncStart)
	return err
}

// Pull synchronizes changes from the remote provider to the local provider.
// It returns true if any changes were pulled.
func (s *Syncer) Pull(ctx context.Context, syncStart time.Time) (bool, error) {
	return s.oneWaySync(ctx, s.remote, s.local, syncStart)
}

// oneWaySync performs a unidirectional synchronization from the source provider to the destination provider.
// It handles creation, updates, and deletions of lists and items based on modification timestamps and status.
// It returns true if any changes were applied to the destination.
func (s *Syncer) oneWaySync(ctx context.Context, src, dst Provider, syncStart time.Time) (bool, error) {
	srcState, err := s.buildProviderState(ctx, src)
	if err != nil {
		return false, err
	}

	dstState, err := s.buildProviderState(ctx, dst)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}

		dstState = &providerState{
			provider: dst,
			lists:    []model.List{},
			listsMap: make(map[string]*model.List),
			itemsMap: make(map[string]*model.Item),
		}
	}

	ss := &syncSession{
		getKey:    s.getKey,
		srcState:  srcState,
		dstState:  dstState,
		syncStart: syncStart,
	}

	changed := false
	for _, srcList := range srcState.lists {
		created, err := ss.syncListCreation(ctx, &srcList)
		changed = changed || created
		if err != nil {
			return changed, err
		}

		updated, err := ss.syncListUpdate(ctx, &srcList)
		changed = changed || updated
		if err != nil {
			return changed, err
		}
	}

	for _, srcList := range srcState.lists {
		if err := ss.syncNextActionProjectUpdate(ctx, &srcList); err != nil {
			return changed, err
		}
	}

	for _, dstList := range dstState.lists {
		deleted, err := ss.syncListDeletion(ctx, &dstList)
		changed = changed || deleted
		if err != nil {
			return changed, err
		}
	}

	return changed, nil
}

// buildProviderState retrieves all lists and items from a provider and builds lookup maps.
func (s *Syncer) buildProviderState(ctx context.Context, p Provider) (*providerState, error) {
	lists, err := p.ListLists(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve lists from provider: %w", err)
	}

	listsMap := make(map[string]*model.List, len(lists))
	itemsMap := make(map[string]*model.Item)
	for _, list := range lists {
		for _, item := range list.Items {
			itemKey := s.getKey(item)
			if itemKey == "" {
				continue
			}

			itemsMap[itemKey] = item
		}

		listKey := s.getKey(&list)
		if listKey == "" {
			continue
		}

		listsMap[listKey] = &list
	}

	state := &providerState{
		provider: p,
		lists:    lists,
		listsMap: listsMap,
		itemsMap: itemsMap,
	}

	return state, nil
}

// providerState holds a set of Resources from a Provider and the provider itself.
type providerState struct {
	provider Provider
	lists    []model.List
	listsMap map[string]*model.List
	itemsMap map[string]*model.Item
}

// syncSession encapsulates the state required for a single one-way synchronization pass.
type syncSession struct {
	getKey    func(model.Resource) string
	srcState  *providerState
	dstState  *providerState
	syncStart time.Time
}

// syncListCreation processes a single list from the source provider state, creating or updating it and its items
// in the destination provider state as needed. It returns true if any changes were applied.
func (ss *syncSession) syncListCreation(ctx context.Context, srcList *model.List) (bool, error) {
	if srcList.Status == model.StatusDeleted {
		return false, nil
	}

	created := false
	listKey := ss.getKey(srcList)
	dstList, ok := ss.dstState.listsMap[listKey]
	if !ok {
		if err := ss.createList(ctx, srcList); err != nil {
			return created, err
		}

		createdList := *srcList
		createdList.Items = make([]*model.Item, 0, len(srcList.Items))
		dstList = &createdList
		listKey = ss.getKey(dstList)
		ss.dstState.listsMap[listKey] = dstList

		created = true
	}

	prevItemID := ""
	for _, srcItem := range srcList.Items {
		if srcItem.Status == model.StatusDeleted {
			continue
		}

		srcItem.ListID = srcList.ID
		srcItem.ExternalListID = srcList.ExternalID

		itemKey := ss.getKey(srcItem)
		dstItem, ok := ss.dstState.itemsMap[itemKey]
		if !ok {
			if err := ss.createItem(ctx, srcItem, prevItemID); err != nil {
				return created, err
			}

			idx := slices.IndexFunc(dstList.Items, func(i *model.Item) bool {
				return ss.getKey(i) == prevItemID
			})

			dstItem = srcItem
			dstList.Items = slices.Insert(dstList.Items, idx+1, dstItem)

			itemKey = ss.getKey(srcItem)
			ss.dstState.itemsMap[itemKey] = dstItem

			created = true
		}

		if srcList.Contains(dstItem) {
			prevItemID = ss.getKey(srcItem)
		}
	}

	return created, nil
}

// syncListUpdate evaluates an existing list and its items from the source provider state,
// updating them in the destination provider if the source is newer or structurally different.
func (ss *syncSession) syncListUpdate(ctx context.Context, srcList *model.List) (bool, error) {
	if srcList.Status == model.StatusDeleted {
		return false, nil
	}

	updated := false
	listKey := ss.getKey(srcList)
	dstList := ss.dstState.listsMap[listKey]
	if (!srcList.Modified.Before(dstList.Modified) && !srcList.Equivalent(dstList)) ||
		srcList.Position != dstList.Position {
		if err := ss.updateList(ctx, srcList, dstList); err != nil {
			return updated, err
		}

		updated = true
	}

	for _, srcItem := range srcList.Items {
		if srcItem.Status == model.StatusDeleted {
			continue
		}

		itemKey := ss.getKey(srcItem)
		dstItem := ss.dstState.itemsMap[itemKey]
		if !srcItem.Modified.Before(dstItem.Modified) && !srcItem.Equivalent(dstItem) {
			if err := ss.updateItem(ctx, srcItem, dstItem); err != nil {
				return updated, err
			}

			updated = true
		} else if dstItem.ProjectID != nil {
			srcItem.ProjectID = dstItem.ProjectID
		}
	}

	return updated, nil
}

func (ss *syncSession) syncNextActionProjectUpdate(ctx context.Context, srcList *model.List) error {
	if strings.HasPrefix(srcList.Name, model.ListProjects) {
		return nil
	}

	for _, srcItem := range srcList.Items {
		if srcItem.ProjectTag == nil && srcItem.ExternalProjectID == nil {
			continue
		}

		if srcItem.ProjectID != nil {
			continue
		}

		itemKey := ss.getKey(srcItem)
		dstItem := ss.dstState.itemsMap[itemKey]
		if err := ss.updateItem(ctx, srcItem, dstItem); err != nil {
			return err
		}
	}

	return nil
}

// syncListDeletion processes a single list from the destination provider state, deleting it or its items
// if they no longer exist in the source provider state or have been marked as deleted.
// It returns true if any changes were applied.
func (ss *syncSession) syncListDeletion(ctx context.Context, dstList *model.List) (bool, error) {
	if dstList.Status == model.StatusDeleted {
		return false, nil
	}

	listKey := ss.getKey(dstList)
	if listKey == "" {
		return false, nil
	}

	srcList, ok := ss.srcState.listsMap[listKey]
	if (!ok || srcList.Status == model.StatusDeleted) && dstList.Modified.Before(ss.syncStart) {
		if err := ss.deleteList(ctx, srcList, dstList); err != nil {
			return false, err
		}

		return true, nil
	}

	deleted := false

	for _, dstItem := range dstList.Items {
		if dstItem.Status == model.StatusDeleted {
			continue
		}

		itemKey := ss.getKey(dstItem)
		if itemKey == "" {
			continue
		}

		srcItem, ok := ss.srcState.itemsMap[itemKey]
		if (!ok || srcItem.Status == model.StatusDeleted) && dstItem.Modified.Before(ss.syncStart) {
			if err := ss.deleteItem(ctx, srcItem, dstItem); err != nil {
				return deleted, err
			}

			deleted = true
		}
	}

	return deleted, nil
}

// createList creates a new list in the destination provider and backfills the external ID
// into the source provider if necessary.
func (ss *syncSession) createList(ctx context.Context, list *model.List) error {
	listKey := ss.getKey(list)
	if err := ss.dstState.provider.CreateList(ctx, list); err != nil {
		return fmt.Errorf("failed to create list %q in destination: %w", list.Name, err)
	}

	if listKey == "" {
		if err := ss.srcState.provider.UpdateList(ctx, list, list); err != nil {
			return fmt.Errorf("failed to backfill external key for list %q in source: %w", list.Name, err)
		}
	}

	return nil
}

// createItem creates a new item in the destination provider and backfills the external ID
// into the source provider if necessary.
func (ss *syncSession) createItem(ctx context.Context, item *model.Item, prevItemID string) error {
	if item.Status == model.StatusOpen {
		item.Status = model.StatusNotStarted
	}

	itemKey := ss.getKey(item)
	if err := ss.dstState.provider.CreateItem(ctx, item, prevItemID); err != nil {
		return fmt.Errorf("failed to create item %q in destination: %w", item.Title, err)
	}

	if itemKey == "" {
		if err := ss.srcState.provider.UpdateItem(ctx, item); err != nil {
			return fmt.Errorf("failed to backfill external key for item %q in source: %w", item.Title, err)
		}
	}

	return nil
}

// updateList updates an existing list in the destination provider, maintaining its position and metadata.
func (ss *syncSession) updateList(ctx context.Context, list, dstList *model.List) error {
	syncList := *list
	listItems := make([]*model.Item, 0, len(syncList.Items))
	for _, item := range syncList.Items {
		if item.Status == model.StatusDeleted {
			continue
		}

		listItem := *item

		itemKey := ss.getKey(item)
		dstItem := ss.dstState.itemsMap[itemKey]
		listItem.ListID = dstItem.ListID
		listItem.ExternalListID = dstItem.ExternalListID

		if listItem.Status == model.StatusOpen {
			listItem.Status = model.StatusNotStarted
			if dstItem.Status == model.StatusInProgress {
				listItem.Status = model.StatusInProgress
			}
		}

		listItems = append(listItems, &listItem)
	}

	syncList.Items = listItems
	if err := ss.dstState.provider.UpdateList(ctx, &syncList, dstList); err != nil {
		return fmt.Errorf("failed to update list %q in destination: %w", syncList.Name, err)
	}

	return nil
}

// updateItem updates an existing item in the destination provider, taking care to preserve specific status transitions.
func (ss *syncSession) updateItem(ctx context.Context, srcItem, dstItem *model.Item) error {
	if srcItem.Status == model.StatusOpen {
		srcItem.Status = model.StatusNotStarted
		if dstItem.Status == model.StatusInProgress {
			srcItem.Status = model.StatusInProgress
		}
	}

	if err := ss.dstState.provider.UpdateItem(ctx, srcItem); err != nil {
		return fmt.Errorf("failed to update item %q in destination: %w", srcItem.Title, err)
	}

	return nil
}

// deleteList handles the deletion of a list. It permanently deletes the list if it exists in the source as deleted,
// or marks it deleted in the destination if it is missing from the source.
func (ss *syncSession) deleteList(ctx context.Context, srcList, dstList *model.List) error {
	if srcList == nil {
		dstList.Status = model.StatusDeleted
		if err := ss.dstState.provider.UpdateList(ctx, dstList, dstList); err != nil {
			return fmt.Errorf("failed to mark list %q as deleted in destination: %w", dstList.Name, err)
		}

		return nil
	}

	if err := ss.dstState.provider.DeleteList(ctx, dstList); err != nil {
		return fmt.Errorf("failed to permanently delete list %q from destination: %w", dstList.Name, err)
	}

	if err := ss.srcState.provider.DeleteList(ctx, srcList); err != nil {
		return fmt.Errorf("failed to permanently delete list %q from source: %w", srcList.Name, err)
	}

	return nil
}

// deleteItem handles the deletion of an item. It permanently deletes the item if it exists in the source as deleted,
// or marks it deleted in the destination if it is missing from the source.
func (ss *syncSession) deleteItem(ctx context.Context, srcItem, dstItem *model.Item) error {
	if srcItem == nil {
		dstItem.Status = model.StatusDeleted
		if err := ss.dstState.provider.UpdateItem(ctx, dstItem); err != nil {
			return fmt.Errorf("failed to mark item %q as deleted in destination: %w", dstItem.Title, err)
		}

		return nil
	}

	if err := ss.dstState.provider.DeleteItem(ctx, dstItem); err != nil {
		return fmt.Errorf("failed to permanently delete item %q from destination: %w", dstItem.Title, err)
	}

	if err := ss.srcState.provider.DeleteItem(ctx, srcItem); err != nil {
		return fmt.Errorf("failed to permanently delete item %q from source: %w", srcItem.Title, err)
	}

	return nil
}

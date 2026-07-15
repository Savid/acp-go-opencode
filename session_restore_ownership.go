package opencodeacp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/savid/acp-go-opencode/internal/opencode"
)

const (
	restoreOwnershipFileName = "acp-go-opencode-restore-ownership-v1.json"
	restoreOwnershipFormat   = "opencode-restore-ownership-v1"
)

var (
	restoreReadFile    = os.ReadFile
	restoreMkdirAll    = os.MkdirAll
	restoreJSONMarshal = json.Marshal
	restoreCreateTemp  = os.CreateTemp
	restoreChmod       = (*os.File).Chmod
	restoreWrite       = (*os.File).Write
	restoreSync        = (*os.File).Sync
	restoreClose       = (*os.File).Close
	restoreRename      = os.Rename
	restoreOpen        = os.Open
)

type restoreOwnershipFile struct {
	Format     string                      `json:"format"`
	Aggregates map[string]restoreOwnership `json:"aggregates"`
}

type restoreOwnership struct {
	SessionID              string `json:"sessionId"`
	RestoreGeneration      string `json:"restoreGeneration"`
	SourceAggregateID      string `json:"sourceAggregateId"`
	DestinationAggregateID string `json:"destinationAggregateId"`
}

func recordSnapshotOwnership(client opencode.Client, snapshot stateSnapshot) error {
	if client.XDGDirs().State == "" {
		return fmt.Errorf("restore ownership state directory is empty")
	}

	registry, err := readRestoreOwnership(client)
	if err != nil {
		return err
	}

	for _, node := range snapshot.Graph {
		registry.Aggregates[node.NativeSessionID] = ownershipFor(snapshot, node)
	}

	return writeRestoreOwnership(client, registry)
}

func claimRestoreOwnership(client opencode.Client, snapshot stateSnapshot, existing map[string][]opencode.SyncEvent) error {
	if client.XDGDirs().State == "" {
		return fmt.Errorf("restore ownership state directory is empty")
	}

	registry, err := readRestoreOwnership(client)
	if err != nil {
		return err
	}

	changed := false

	for _, node := range snapshot.Graph {
		expected := ownershipFor(snapshot, node)

		owner, found := registry.Aggregates[node.NativeSessionID]
		if found {
			if owner != expected {
				return fmt.Errorf("destination aggregate %q is owned by another restore", node.NativeSessionID)
			}

			continue
		}

		if len(existing[node.NativeSessionID]) != 0 {
			return fmt.Errorf("destination aggregate %q has no durable restore owner", node.NativeSessionID)
		}

		registry.Aggregates[node.NativeSessionID] = expected
		changed = true
	}

	if changed {
		return writeRestoreOwnership(client, registry)
	}

	return nil
}

func verifyRestoreOwnership(client opencode.Client, snapshot stateSnapshot, node stateSnapshotNode) error {
	if client.XDGDirs().State == "" {
		return fmt.Errorf("restore ownership state directory is empty")
	}

	registry, err := readRestoreOwnership(client)
	if err != nil {
		return err
	}

	if registry.Aggregates[node.NativeSessionID] != ownershipFor(snapshot, node) {
		return fmt.Errorf("destination aggregate %q lost durable restore ownership", node.NativeSessionID)
	}

	return nil
}

func ownershipFor(snapshot stateSnapshot, node stateSnapshotNode) restoreOwnership {
	return restoreOwnership{
		SessionID: node.SessionID, RestoreGeneration: snapshot.RestoreGeneration,
		SourceAggregateID: node.NativeSessionID, DestinationAggregateID: node.NativeSessionID,
	}
}

func readRestoreOwnership(client opencode.Client) (restoreOwnershipFile, error) {
	path := filepath.Join(client.XDGDirs().State, restoreOwnershipFileName)

	data, err := restoreReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return restoreOwnershipFile{Format: restoreOwnershipFormat, Aggregates: map[string]restoreOwnership{}}, nil
	}

	if err != nil {
		return restoreOwnershipFile{}, fmt.Errorf("read restore ownership: %w", err)
	}

	var registry restoreOwnershipFile
	if err := json.Unmarshal(data, &registry); err != nil {
		return restoreOwnershipFile{}, fmt.Errorf("decode restore ownership: %w", err)
	}

	if registry.Format != restoreOwnershipFormat || registry.Aggregates == nil {
		return restoreOwnershipFile{}, fmt.Errorf("invalid restore ownership registry")
	}

	return registry, nil
}

func writeRestoreOwnership(client opencode.Client, registry restoreOwnershipFile) error {
	directory := client.XDGDirs().State
	if err := restoreMkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create restore ownership directory: %w", err)
	}

	data, err := restoreJSONMarshal(registry)
	if err != nil {
		return err
	}

	temporary, err := restoreCreateTemp(directory, ".restore-ownership-*")
	if err != nil {
		return fmt.Errorf("create restore ownership temporary file: %w", err)
	}

	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	chmodErr := restoreChmod(temporary, 0o600)
	if chmodErr != nil {
		_ = restoreClose(temporary)

		return chmodErr
	}

	_, writeErr := restoreWrite(temporary, data)
	if writeErr != nil {
		_ = restoreClose(temporary)

		return writeErr
	}

	syncErr := restoreSync(temporary)
	if syncErr != nil {
		_ = restoreClose(temporary)

		return syncErr
	}

	closeErr := restoreClose(temporary)
	if closeErr != nil {
		return closeErr
	}

	path := filepath.Join(directory, restoreOwnershipFileName)

	renameErr := restoreRename(temporaryPath, path)
	if renameErr != nil {
		return fmt.Errorf("publish restore ownership: %w", renameErr)
	}

	dir, err := restoreOpen(directory)
	if err != nil {
		return err
	}

	err = restoreSync(dir)

	return errors.Join(err, restoreClose(dir))
}

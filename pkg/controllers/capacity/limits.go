package capacity

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
)

const (
	maxStatusBytes  = 256 * 1024
	maxMessageBytes = 1024
	listFieldBytes  = len(`,"launchBackoffs":[]`) + len(`,"sleds":[]`) + len(`,"placement":[]`)
)

func limitStatus(status *v1alpha1.OxideCapacityStatus) error {
	backoffs, sleds := status.LaunchBackoffs, status.Inventory.Sleds
	status.LaunchBackoffs, status.Inventory.Sleds = nil, nil
	status.OmittedLaunchBackoffCount += len(backoffs)
	status.Inventory.OmittedSledCount += len(sleds)
	for i := range backoffs {
		truncateMessage(&backoffs[i].Message, &backoffs[i].MessageTruncated)
	}
	var placement []v1alpha1.PlacementFailure
	if history := status.History; history != nil {
		placement, history.Placement = history.Placement, nil
		history.OmittedPlacementCount += len(placement)
		for i := range placement {
			truncateMessage(&placement[i].Message, &placement[i].MessageTruncated)
		}
		if storage := history.Storage; storage != nil {
			truncateMessage(&storage.Message, &storage.MessageTruncated)
		}
	}
	summary, err := json.Marshal(status)
	if err != nil {
		return err
	}
	remaining := maxStatusBytes - len(summary) - listFieldBytes
	if remaining < 0 {
		return fmt.Errorf("capacity status summary exceeds %d bytes", maxStatusBytes)
	}
	status.LaunchBackoffs, err = fitEntries(backoffs, &remaining)
	if err != nil {
		return err
	}
	status.OmittedLaunchBackoffCount -= len(status.LaunchBackoffs)
	status.Inventory.Sleds, err = fitEntries(sleds, &remaining)
	if err != nil {
		return err
	}
	status.Inventory.OmittedSledCount -= len(status.Inventory.Sleds)
	if history := status.History; history != nil {
		history.Placement, err = fitEntries(placement, &remaining)
		if err != nil {
			return err
		}
		history.OmittedPlacementCount -= len(history.Placement)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		return err
	}
	if len(encoded) > maxStatusBytes {
		return fmt.Errorf("capacity status exceeds %d bytes", maxStatusBytes)
	}
	return nil
}

func fitEntries[T any](entries []T, remaining *int) ([]T, error) {
	var retained []T
	for _, entry := range entries {
		encoded, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		cost := len(encoded) + 1
		if cost <= *remaining {
			retained = append(retained, entry)
			*remaining -= cost
		}
	}
	return retained, nil
}

func truncateMessage(message *string, truncated *bool) {
	if len(*message) <= maxMessageBytes {
		return
	}
	end := maxMessageBytes
	for end > 0 && !utf8.RuneStart((*message)[end]) {
		end--
	}
	*message = (*message)[:end]
	*truncated = true
}

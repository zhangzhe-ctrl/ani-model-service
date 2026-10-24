package model

import "fmt"

// NormalizeVersionStatus translates UI labels to persisted version states.
// Deleted versions remain excluded from the public version list.
func NormalizeVersionStatus(status string) (string, error) {
	switch status {
	case "", "pending", "importing", "ready", "error":
		return status, nil
	case "downloading":
		return "importing", nil
	case "failed":
		return "error", nil
	default:
		return "", fmt.Errorf("unsupported model version status %q", status)
	}
}

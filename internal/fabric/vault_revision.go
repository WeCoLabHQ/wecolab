package fabric

import (
	"fmt"
	"strconv"
	"strings"
)

func Version(data map[string]string, field string) (uint64, error) {
	raw := data[field]
	if raw == "" || strings.TrimSpace(raw) != raw {
		return 0, fmt.Errorf("%s migration required: missing or malformed", field)
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || strconv.FormatUint(n, 10) != raw {
		return 0, fmt.Errorf("invalid %s %q", field, raw)
	}
	return n, nil
}

func BumpVault(v map[string]string) error {
	if v["retirement-phase"] != "" {
		return fmt.Errorf("vault retirement in progress; wait for writer recovery")
	}
	n, err := Version(v, "mutation-revision")
	if err != nil {
		return err
	}
	if n == ^uint64(0) {
		return fmt.Errorf("vault mutation revision exhausted")
	}
	v["mutation-revision"] = strconv.FormatUint(n+1, 10)
	return nil
}
func NextKeyVersion(v map[string]string) error {
	n, err := Version(v, "key-version")
	if err != nil {
		return err
	}
	if n == ^uint64(0) {
		return fmt.Errorf("vault key version exhausted")
	}
	if err := BumpVault(v); err != nil {
		return err
	}
	v["key-version"] = strconv.FormatUint(n+1, 10)
	return nil
}
func CheckKeyVersion(app, vault map[string]string) error {
	got, err := Version(app, "key-version")
	if err != nil {
		return err
	}
	want, err := Version(vault, "key-version")
	if err != nil {
		return err
	}
	if got > want {
		return fmt.Errorf("app key version %d exceeds vault %d", got, want)
	}
	if got == want && app["b2-key-id"] != vault["b2-key-id"] {
		return fmt.Errorf("same vault version carries different key IDs")
	}
	return nil
}

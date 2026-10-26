package scan

import (
	"encoding/json"
	"fmt"

	"dustkeeper/internal/domain"
)

// jsonIdentity serialises a file identity for storage.
func jsonIdentity(id domain.FileIdentity) (string, error) {
	b, err := json.Marshal(id)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// mustJSONIdentity is used where a marshal failure cannot be recovered from
// meaningfully; the value is a fixed struct of scalars.
func mustJSONIdentity(id domain.FileIdentity) string {
	s, err := jsonIdentity(id)
	if err != nil {
		panic(fmt.Sprintf("dustkeeper: cannot encode file identity: %v", err))
	}
	return s
}

// mustJSONToken serialises a change token for storage.
func mustJSONToken(t domain.ChangeToken) string {
	b, err := json.Marshal(t)
	if err != nil {
		panic(fmt.Sprintf("dustkeeper: cannot encode change token: %v", err))
	}
	return string(b)
}

// DecodeIdentity reads a stored identity. It is used by every reader so that the
// encoding lives in one place.
func DecodeIdentity(s string) (domain.FileIdentity, error) {
	var id domain.FileIdentity
	err := json.Unmarshal([]byte(s), &id)
	return id, err
}

// DecodeToken reads a stored change token.
func DecodeToken(s string) (domain.ChangeToken, error) {
	var t domain.ChangeToken
	err := json.Unmarshal([]byte(s), &t)
	return t, err
}

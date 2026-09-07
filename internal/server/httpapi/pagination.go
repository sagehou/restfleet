package httpapi

import (
	"encoding/base64"
	"net/http"
	"net/url"

	"github.com/google/uuid"
)

// listPage enforces the same cursor/query rules for credential and repository lists.
func (a *API) listPage(w http.ResponseWriter, r *http.Request, requestedLimit *int, cursor *string) (uuid.UUID, int, bool) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	valid := err == nil
	for key, values := range query {
		if (key != "limit" && key != "cursor") || len(values) != 1 {
			valid = false
		}
	}
	if !valid {
		a.problem(w, r, http.StatusBadRequest, "INVALID_QUERY", "Invalid request", "The list query is invalid.", nil)
		return uuid.Nil, 0, false
	}
	limit := 50
	if requestedLimit != nil {
		limit = *requestedLimit
	}
	if limit < 1 || limit > 200 {
		a.problem(w, r, http.StatusBadRequest, "INVALID_LIMIT", "Invalid request", "Limit must be between 1 and 200.", nil)
		return uuid.Nil, 0, false
	}
	after := uuid.Nil
	if cursor != nil {
		raw, err := base64.RawURLEncoding.DecodeString(*cursor)
		if err != nil || len(raw) != 16 {
			a.problem(w, r, http.StatusBadRequest, "INVALID_CURSOR", "Invalid request", "The cursor is invalid.", nil)
			return uuid.Nil, 0, false
		}
		copy(after[:], raw)
	}
	return after, limit, true
}

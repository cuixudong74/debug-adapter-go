package debug

import "strings"

// Scope is a permission scope.
type Scope string

const (
	ScopeReadVariables Scope = "variables:read"
	ScopeReadSensitive Scope = "variables:read_sensitive"
	ScopeEvaluate      Scope = "evaluate"
	ScopeControl       Scope = "session:control"
)

// User identifies the principal driving a session.
type User struct {
	ID    string
	Name  string
	Roles []string
}

// HasScope reports whether the user holds the given scope.
func (u User) HasScope(s Scope) bool {
	for _, r := range u.Roles {
		if r == string(s) {
			return true
		}
	}
	return false
}

// sensitiveNameHeuristic marks fields whose names suggest secrets.
var sensitiveSubstrings = []string{"password", "passwd", "secret", "token", "credential", "private_key", "apikey", "api_key"}

// IsSensitiveName reports whether a variable name looks sensitive.
func IsSensitiveName(name string) bool {
	n := strings.ToLower(name)
	for _, sub := range sensitiveSubstrings {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

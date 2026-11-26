// This file holds the task ID format that the API and the code daemon share.

package codetask

import "regexp"

var taskIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

// ValidTaskID reports whether id is a valid task ID.
func ValidTaskID(id string) bool {
	return taskIDPattern.MatchString(id)
}

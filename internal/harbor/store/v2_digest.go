package store

import (
	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
)

const taskDigestV2Prefix = taskpolicy.TaskDigestV2Prefix

// ValidateTaskDigestV2 is shared by the task-policy boundary and the durable
// store. V1 evidence has no compatible namespace and cannot be persisted as a
// V2 TaskRevision digest.
func ValidateTaskDigestV2(digest string) error {
	return taskpolicy.ValidateV2TaskDigest(digest)
}

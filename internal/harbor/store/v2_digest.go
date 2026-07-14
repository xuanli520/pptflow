package store

import (
	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
)

const taskDigestV2Prefix = taskpolicy.TaskDigestV2Prefix

// ValidateTaskDigestV2 is shared by the task-policy boundary and the durable
// store. Only canonical V2 digests can be persisted as TaskRevision evidence.
func ValidateTaskDigestV2(digest string) error {
	return taskpolicy.ValidateV2TaskDigest(digest)
}

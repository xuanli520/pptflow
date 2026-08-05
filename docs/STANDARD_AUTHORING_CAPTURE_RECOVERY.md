# Standard Authoring Capture Recovery

`authoring.start` seals two immutable records below the managed lifecycle
operation directory before it creates the draft Task, AuthoringSession, or
Run.

1. `deployment-definition.json` binds the prepared operation to its approved
   Standard execution profile and catalog receipt. The deployment lock is
   resolved at runtime against the currently installed deployment.
2. `capture-receipt.json` is published only after the requested Git archive
   has passed archive validation and its bytes have been written to the
   content-addressed object store.

The operation directory is descriptor-rooted and guarded by a per-operation
Unix advisory lock. A retry with a valid capture receipt verifies the stored
object and archive, then continues from that receipt without contacting Git.
Prepared operations without a deployment definition fail closed; they require
a new idempotency key rather than adopting a later deployment definition.

There is one intentionally bounded crash window: a process can terminate after
the immutable object has been published but before `capture-receipt.json` is
durably published. The next retry cannot safely infer which orphan object came
from that capture, so it may perform one additional read-only Git capture. An
orphan object is not an AuthoringSource and cannot be used by a Run without a
validated receipt. Cancellation after object publication continues sealing the
receipt so ordinary cancellation does not enter this window.

// Package codedaemon implements the runner-facing daemon inside a code sandbox.
//
// Invariants that the code does not make obvious:
//
//   - All tasks share one worker. The supervisor starts it on first use and
//     never restarts it. Once it stops, the daemon is unhealthy.
//   - A task deadline stops the shared worker. Other running tasks without a
//     result then end with "crashed".
//   - If a write to the client fails, the handler stops streaming but waits
//     for the task to end before it frees the lane and the task directory.
package codedaemon

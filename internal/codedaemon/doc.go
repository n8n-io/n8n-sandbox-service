// Package codedaemon implements the runner-facing daemon inside a code sandbox.
//
// The package has these components. Each one owns its state.
//
//   - Daemon (daemon.go): the HTTP server, its routes, the client source
//     guard, the health probe, and the execution lanes.
//   - Task handling (handle_task.go, admission.go, staging.go, results.go):
//     one POST /tasks request.
//   - supervisor (supervisor.go): starts the shared runtime on first use and
//     never restarts it.
//   - worker (worker.go, worker_control.go): the runtime process and the
//     control protocol on FD 3.
//   - execution (execution.go): one dispatched task, its result, and its
//     deadline.
//
// The life of one task:
//
//  1. Admission reads the spec part, reserves the task ID, and takes a free
//     lane. If no lane is free, the request gets 429.
//  2. Staging writes the input files to a private task directory.
//  3. The supervisor gets the worker and registers an execution for the task
//     ID on it. Then it sends the start message to the runtime.
//  4. The handler streams heartbeats until the execution has an outcome: the
//     runtime's terminal message, a runtime exit, or the deadline. At the
//     deadline, the execution stops the shared worker.
//  5. The handler sends the terminal event, then removes the task directory
//     and frees the lane.
package codedaemon

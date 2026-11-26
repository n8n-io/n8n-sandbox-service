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
//	POST /tasks
//	     |
//	     v
//	+------------+  invalid spec, duplicate ID, no free lane
//	| admission  |-------------------------------------------> 4xx
//	+------------+
//	     | task ID reserved, lane taken
//	     v
//	+------------+  invalid input, input limit
//	|  staging   |-------------------------------------------> 4xx
//	+------------+
//	     | private task directory
//	     v
//	+------------+  getWorker: start the worker once
//	| supervisor |  worker.register(id, deadline)
//	+------------+  "start" -----------------------------> +---------+
//	     |                                                 | runtime |
//	     v                                                 +---------+
//	+------------+                                              |
//	| execution  |<-- worker.deliver <-- readControl <----------+
//	+------------+     ("done" or "error")
//	     |          watch: result, runtime exit, or deadline
//	     |          (at the deadline, it stops the shared worker)
//	     v
//	stream: accepted, heartbeats, then one terminal event
//	     |
//	     v
//	unregister, remove the task directory, free the lane
package codedaemon

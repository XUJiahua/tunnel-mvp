# Local concurrency patch

Source: `github.com/rancher/remotedialer` at
`v0.6.2-0.20261014155149-1e594259f9fc` (commit `1e594259f9fc`).
The original module metadata, source files, README and Apache-2.0 LICENSE are
retained. The application uses a local Go module replacement; the wire protocol,
public API, DNS/ACL authorizer and dependency versions are unchanged.

`wsconn.go` previously changed Gorilla's write deadline from Ping/Pong reader
callbacks while a data writer could be active. Gorilla's write state is not safe
for that concurrent access. Deadlines now belong to the writer critical section:

- Each data write sets its socket deadline while holding the existing mutex.
- A zero caller deadline uses the existing 60-second ping wait as a bounded
  write timeout; an explicit caller deadline is applied to the actual socket.
- A queued writer checks its context after acquiring the mutex, preventing an
  already expired write from sending data later.
- Reader Ping/Pong callbacks refresh only the read deadline.

Regression tests exercise simultaneous heartbeat/data traffic and verify that a
blocked timed write releases its writer instead of leaving a background write
stuck. `make test` explicitly runs this nested module with `-race` because the
main module's `go test ./...` does not traverse nested modules.

Remove this replacement only after an upstream version contains equivalent
synchronization and passes both these tests and the application end-to-end suite.

# FAQ

**Is there a daemon or a server?** No. Everything is files and `flock(2)` locks under the state
directory, which must be on a local file system (not NFS).

**Does exit 75 mean my command failed?** No. It means the command did not get to run (or, for a
remote job, that the infrastructure failed after it started; a remote job is not retried
automatically). Run it again.

**What happens when a caller is killed?** The kernel drops its locks. The next caller reaps the
tickets and slots it left behind.

**Can a repository raise its own limits?** No. A repo file can only narrow host limits; a host-only
key in a repo file is an error (exit 78).

**How does an agent wait for a job that outlasts its tool-call limit?** Start it with
`bitc run --detach --name build ...` and call `bitc wait build` repeatedly; `wait` returns 124 until
the job ends. See [agent-guide.md](agent-guide.md).

**Does it enforce a memory limit on my command?** No. `memory` is an admission reservation.

**How is it different from `flock`, GNU `parallel` or CI concurrency groups?** See
[comparison.md](comparison.md).

**Where is the full design?** [spec.md](spec.md). The machine-readable status format is
[status.schema.json](status.schema.json).

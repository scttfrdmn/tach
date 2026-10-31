# Security Policy

## Supported versions

Security fixes are made against the latest tagged 0.x release and `main`.

## Reporting a vulnerability

Please report suspected vulnerabilities privately, through
[GitHub private vulnerability reporting](https://github.com/scttfrdmn/tach/security/advisories/new)
(the repository's Security tab → "Report a vulnerability"). Do **not** open a public issue for
a security report.

Please include a description, reproduction steps, and the affected version or commit. We will
acknowledge receipt within a few business days and keep you informed of progress.

## Scope notes

tach runs on the machine being measured, usually with the privileges of the user running the
job. What it touches:

- **It executes `dcgmi`** — the first one on `PATH`, or the path given with `--dcgmi`. Treat
  that path as trusted code. tach parses `dcgmi dmon` output and does not otherwise talk to
  `nv-hostengine`.
- **It executes the wrapped command** with tach's own environment, stdin, stdout and stderr,
  and forwards SIGINT and SIGTERM to it. tach adds no privilege; run it as the user who should
  run the job.
- **It makes one network request**: EC2 instance metadata at the link-local `169.254.169.254`
  (IMDSv2, 400 ms timeout, proxies bypassed) to read the instance type. `--no-imds` or
  `--instance` disables it.
- **It writes a CSV and a summary JSON** to the working directory, or to `--csv`. The summary
  records the full command line and the hostname; don't put secrets in arguments you measure,
  and review summaries before sharing them.

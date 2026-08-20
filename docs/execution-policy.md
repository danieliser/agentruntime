# Execution policy

AgentD's execution policy is a versioned, canonical authority grant. The
canonical JSON is covered by `effective_policy_sha256`; callers should compare
the returned policy and hash with the request they approved before dispatch.

## Supported versions

- `2.0` is the AgentD 2.2.0 compatibility profile. Its resource ceilings are
  fixed and implicit: 2 GiB memory, 2 CPU cores, 256 PIDs, and 1,024 open files.
- `2.1` adds a hash-covered `resources` object. Omitting the object
  canonicalizes it to the defaults below. A caller may request a tighter value
  within the advertised range but cannot widen AgentD's maximum.

Both versions accept the additive, hash-covered `egress_diagnostics` boolean.
It defaults to `false`. When a caller opts in and daemon diagnostics are
enabled, AgentD retains only UTC timestamps and attempted HTTPS CONNECT host
names in the private diagnostic directory; request payloads and headers never
enter this record. The ordinary diagnostic retention/disable controls and
`0700`/`0600` modes apply. The effective policy returned with the session shows
whether the mode was enabled.

```json
{
  "resources": {
    "memory_bytes": 2147483648,
    "cpu_cores": 2,
    "pids": 256,
    "open_files": 1024
  }
}
```

| Limit | Minimum | Default / maximum | Docker enforcement |
| --- | ---: | ---: | --- |
| Memory | 67,108,864 bytes | 2,147,483,648 bytes | cgroup hard limit (`--memory`) |
| CPU | 0.1 cores | 2 cores | cgroup quota (`--cpus`) |
| PIDs | 16 | 256 | cgroup process limit (`--pids-limit`) |
| Open files | 64 | 1,024 | soft and hard `RLIMIT_NOFILE` (`--ulimit`) |

Zero, negative, non-finite, below-minimum, or above-maximum values fail before
session admission with the stable `resource_limit_exceeded` error. Restricted
requests cannot use the legacy `container.memory` or `container.cpus` fields to
bypass the policy.

Docker OOM proof (`OOMKilled=true`) produces a typed
`session.resource_limit_exceeded` terminal event and an exit result with
`FailureReason=resource_limit_exceeded`. The retained terminal receipt contract
does not change: the receipt remains `state=failed, reason=failed`, with exit
code 137 and `SIGKILL` proof. CPU quota is throttling rather than a terminal
breach; PID and FD exhaustion are synchronously refused by the kernel to the
provider process.

The authenticated `/api/v1/capabilities` response is the source of truth for
supported policy versions, defaults, minimums, maximums, and the stable breach
code. Its egress section also advertises the diagnostic policy field, default,
and retained record fields.

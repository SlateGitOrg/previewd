# Threat model

previewd sits between an SCM (untrusted network input) and the Kubernetes
API (privileged writes). This document lists what it defends against, how,
and what it does not cover.

## Assets

* The cluster: previewd's ServiceAccount can create namespaces and write
  namespaced objects (including Secrets) cluster-wide.
* The SCM token (commit-status write).
* The webhook secrets.
* Other tenants' namespaces.

## Trust boundaries and actors

| Actor | Trust | Can influence |
|---|---|---|
| Anyone on the network | none | HTTP requests to the webhook endpoint |
| GitHub (holder of the webhook secret) | authenticated sender | event payloads |
| PR author (any contributor, or a fork author) | untrusted | branch names, titles, labels (if they can label), head SHA, draft flag |
| Template-set author (platform team) | trusted | manifests, quotas, allowlists |
| Cluster admin | trusted | RBAC, CRD, PreviewEnvironment objects |

## Threats and mitigations

| # | Threat | Mitigation | Evidence |
|---|---|---|---|
| T1 | Forged webhook creates or tears down previews | HMAC-SHA256 over the raw body, verified before parsing; constant-time compare; all secrets tried; 401 on failure | `TestSignAndVerify`, `FuzzVerifyNeverPanics`, demo step 1 |
| T2 | Replay of a captured valid delivery (GitHub signatures carry no timestamp) | Delivery-ID cache short-circuits recent replays; the intake ignores events older than the stored `lastActivityTime`, so a replayed "opened" cannot resurrect a closed PR and a replayed old push cannot roll back the SHA | `TestLifecycleOfEvents` (stale), `TestDeliveryCache` |
| T3 | Oversized bodies / slowloris | 1 MiB body cap (configurable), server read/header/write timeouts | `TestHandlerBodyLimit`, `internal/webhook/server.go` |
| T4 | Template injection through PR text (title with newlines creating a ClusterRoleBinding) | `quote` JSON-encodes values; post-render validation rejects any non-allowlisted kind, any object outside the preview namespace, RBAC kinds (never allowable) | `TestTitleInjectionIsNeutralised`, `TestUnquotedInjectionIsCaughtByValidation` |
| T5 | PR code escaping to the node | Pod templates may not use hostNetwork/hostPID/hostIPC, hostPath, hostPort, privileged, or SYS_ADMIN/NET_ADMIN/ALL capabilities; namespace gets Pod Security Admission `enforce` (baseline by default) | `TestValidationRejections` |
| T6 | Preview reaching other namespaces / the internet | Default-deny ingress; optional default-deny egress allowing only DNS, same namespace and listed CIDRs | `TestRenderDefaultShape`, golden files |
| T7 | Resource exhaustion / cost (many PRs, big pods, cloud LBs) | ResourceQuota + LimitRange per namespace; ClusterIP-only Services by default; TTL on idle PRs; retention deletes records | `TestValidationRejections` (LoadBalancer), `TestTTLExpiryAndRevivalOnPush` |
| T8 | Squatting: a PR whose computed namespace equals an existing one (e.g. `pr-x-1` created by someone else) | Namespace must carry previewd's managed-by and instance labels; otherwise provisioning fails and teardown never deletes it | `TestForeignNamespaceIsNeverAdoptedOrDeleted` |
| T9 | Fork PRs running untrusted code with in-cluster credentials | Fork PRs ignored by default (`--allow-forks` to opt in) | `TestParseFixtures` |
| T10 | Unwanted repositories using the controller | Repository rules (`--repos owner/*`); everything else is acknowledged and ignored | `TestRepositoryRules` |
| T11 | Secret leakage | Secrets only from env/files (never flags); token sent only to the configured API URL; secrets never logged | `internal/config`, `internal/scm/github.go` |
| T12 | Label-gating bypass | `--required-label` is enforced by the reconciler from the stored labels, so any event (labeled, synchronize, edited) is evaluated against current labels | `TestRequiredLabelGatesPreview` |

## Residual risks (not mitigated)

* **Broad RBAC.** Namespaces are dynamic, so the ClusterRole grants
  cluster-wide create/update/delete on the namespaced kinds templates may
  contain — including Secrets. A compromise of previewd is a compromise of
  every namespace for those kinds. Mitigations to consider: an admission
  policy (e.g. ValidatingAdmissionPolicy) restricting previewd's
  ServiceAccount to namespaces labelled `app.kubernetes.io/managed-by=previewd`;
  removing Secrets from the allowlist.
* **What runs in the preview is the PR's code.** previewd limits where it
  runs, not what it does inside its namespace. Restricted egress is opt-in.
* **Labels are trusted.** Anyone who can label PRs can enable label-gated
  previews and label-gated template sections.
* **The delivery cache is per replica and in memory.** Replays of very old
  deliveries are still blocked by the `updated_at` ordering, but a replay of
  the most recent event for a PR is accepted (it is a no-op by
  construction).
* **No mTLS or IP allowlisting** of the webhook endpoint; put it behind an
  ingress that restricts source ranges if required.
* **The GitHub token** is a long-lived token in a Secret; a GitHub App with
  short-lived installation tokens would be better (roadmap).

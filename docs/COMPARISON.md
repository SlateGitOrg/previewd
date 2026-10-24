# previewd vs. the Argo CD ApplicationSet Pull Request generator

The most common way to get per-PR environments on Kubernetes today is Argo
CD's ApplicationSet controller with the **Pull Request generator**. This page
compares the two honestly. It describes Argo CD from its public
documentation (ApplicationSet "Pull Request Generator", "Webhook", "Sync
Options" pages, Argo CD 2.x/3.x); behaviour can change between releases, so
check the docs for the version you run. previewd's column describes this
repository and links to the test that demonstrates each claim.

## How each one works

**ApplicationSet PR generator.** An `ApplicationSet` declares an SCM provider
(GitHub, GitLab, Gitea, Bitbucket Server/Cloud, Azure DevOps, ...) and a
template `Application`. The controller lists open PRs through the provider's
API — polling every `requeueAfterSeconds` (the docs give 30 minutes as the
default), or sooner when an ApplicationSet webhook delivery arrives — and
renders one `Application` per PR with parameters such as `number`, `branch`,
`branch_slug`, `head_sha`, `head_short_sha` and labels. When a PR closes (or
stops matching filters) its `Application` is removed; whether its resources
are pruned depends on the Application finalizer and
`preserveResourcesOnDeletion`. Each `Application` is then synced by Argo CD
proper, with all of Argo's machinery: Helm, Kustomize, plugins, sync waves,
health checks for many kinds, drift detection, the UI, multi-cluster.

**previewd.** A signed GitHub webhook is the only input. The intake writes a
`PreviewEnvironment` per PR; a level-triggered reconciler renders a template
set into a namespace plus guard rails and app manifests, server-side applies
them, prunes leftovers, waits for workloads, posts commit statuses, and tears
everything down on close, label removal or TTL expiry.

## Feature comparison

| Concern | ApplicationSet PR generator | previewd |
|---|---|---|
| Trigger | Polls the SCM API on an interval; webhook can trigger an early refresh | Push only: signed `pull_request` webhooks (`internal/webhook`) |
| Webhook authentication | Argo CD validates webhook secrets for supported providers | HMAC-SHA256 with secret rotation, constant-time compare (`TestSignAndVerify`, `TestKnownVector`) |
| Out-of-order / replayed deliveries | Not applicable: the generator re-lists PRs, so it always sees current state | Ordered by PR `updated_at`; older events are `stale` and never written (`TestLifecycleOfEvents`, demo step 3) |
| What is rendered | Any Argo CD source: Helm, Kustomize, plain YAML, plugins | Go `text/template` YAML only (`internal/render`) |
| Namespace lifecycle | `CreateNamespace=true` creates it; Argo CD documents that it does **not** delete namespaces it created this way, so per-PR namespaces need separate cleanup | Namespace is owned (owner reference + finalizer) and deleted on close/TTL/delete (`TestCloseTearsDownThenRetentionDeletesRecord`) |
| Quota / NetworkPolicy / PSA per environment | Whatever the manifests include (`managedNamespaceMetadata` can add labels/annotations) | Always generated from the template set config: ResourceQuota, LimitRange, default-deny + allow rules, optional egress lockdown, PSA labels (`TestRenderDefaultShape`) |
| Guard rails on rendered output | Argo CD Projects restrict destinations and kinds per project | Per-template kind allowlist (RBAC and cluster-scoped kinds never allowed), namespace pinning, pod hostPath/hostNetwork/privileged checks, ClusterIP-only Services by default (`TestValidationRejections`) |
| Untrusted PR text in templates | Parameters are substituted into the Application template; quoting is the author's job | `quote` helper plus post-render validation as a backstop (`TestTitleInjectionIsNeutralised`, `TestUnquotedInjectionIsCaughtByValidation`) |
| TTL for idle PRs | No built-in TTL: an environment exists while the PR is open and matches | TTL from last PR activity; wakes exactly at expiry; a new push revives it (`TestTTLExpiryAndRevivalOnPush`, demo steps 5–6) |
| Commit status on the PR | Not built into the generator; Argo CD Notifications can post commit statuses | Built in: pending / success / failure / expired / removed, de-duplicated per SHA (`internal/scm`, demo step 10) |
| Fork PRs | Configurable per provider / filters | Ignored unless `--allow-forks` (`TestParseFixtures`) |
| Gating | `branchMatch`, label filters | Required label, draft skipping, repository rules (`TestRequiredLabelGatesPreview`) |
| Health | Rich health assessment for many kinds, custom Lua health checks | Deployments and StatefulSets only (rollout complete) |
| Drift / self-heal | Yes (automated sync with self-heal) | Re-applies on every bundle change and on each reconcile that finds a hash mismatch; does not detect out-of-band edits to unchanged bundles |
| UI, multi-cluster, RBAC model | Yes | No UI; single cluster; Kubernetes RBAC only |
| Moving parts | Argo CD + ApplicationSet controller (+ Notifications for statuses) | One binary, one CRD |
| Maturity | Widely used CNCF project | Portfolio project; never run against a production cluster |

## When to use which

Choose **Argo CD ApplicationSets** if you already run Argo CD, need Helm or
Kustomize, want a UI and drift self-healing, deploy to several clusters, or
need health checks beyond Deployments. Add Notifications for commit statuses
and a separate job for namespace and TTL cleanup.

previewd is a better fit when the preview problem is mostly *lifecycle and
safety*: environments that must disappear when idle, namespaces that must be
cleaned up, PR-controlled input that must not escape its namespace, and
statuses that reviewers see on the PR, all without operating Argo CD. Its
design choices — push-driven intake that only writes desired state, a pure
level-triggered state machine, server-side apply with label-based pruning —
are also what one would borrow if building such a controller on top of an
existing GitOps tool.

## Could they be combined?

Yes. previewd's renderer could emit an Argo CD `Application` instead of raw
manifests (an `argoproj.io/v1alpha1/Application` kind in the allowlist would
need to target the Argo CD namespace, which the namespace-pinning rule
currently forbids). That is listed on the roadmap rather than implemented.

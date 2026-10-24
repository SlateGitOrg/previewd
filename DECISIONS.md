# Decisions

Rulings made while building previewd, in the format
`- Ruling: <what> — <why> — <cost if wrong>`.

## API and data model

- Ruling: one cluster-scoped `PreviewEnvironment` CRD per pull request, named `<owner>-<repo>-pr-<n>` — a cluster-scoped owner can hold a controller owner reference on the Namespace it creates (namespaced owners cannot), and a deterministic name lets the webhook and the reconciler find each other without a lookup table — if multi-tenant installs need per-team scoping, a namespaced variant with a separate namespace registry would be needed.
- Ruling: the webhook intake only writes `spec`; all side effects happen in the reconciler — keeps webhook latency low and makes every side effect retryable and level-triggered — if users expect the webhook response to say whether the environment came up, they will have to read status or the commit status instead.
- Ruling: order events by the PR's `updated_at` (stored as `spec.lastActivityTime`), ignoring older events as `stale` — GitHub delivers asynchronously and redeliveries can arrive late; `updated_at` is monotonic per PR and present on every pull_request event — equal-second events are accepted in arrival order; if two different events share a second and arrive reversed, the older one wins until the next event.
- Ruling: TTL = `lastActivityTime + (spec.ttl | template ttl | 72h)`; any PR activity extends it — matches "idle previews should go away" without a separate heartbeat — PRs with long review gaps and no pushes will expire and need a push (or a manual `spec.ttl`) to come back.
- Ruling: Closed/Expired records are kept for `--retention` (24h default) and then delete themselves — keeps `kubectl get pe` useful for debugging recent teardowns without growing forever — operators who want history beyond 24h must raise the flag or rely on Events/metrics.

## Controller behaviour

- Ruling: the state machine is a pure function (`lifecycle.Decide`) separate from the reconciler — it can be property-tested exhaustively without a cluster, and the reconciler shrinks to observe/act — the observation struct must be kept complete; a fact read by the reconciler but not passed to Decide would break level-triggering.
- Ruling: release the teardown finalizer as soon as the namespace is Terminating, not when it is fully gone — the namespace controller guarantees content cleanup, and envtest (no namespace controller) would otherwise hang forever — a namespace stuck Terminating (e.g. on a broken finalizer of some other controller) is no longer visible through the PreviewEnvironment.
- Ruling: never adopt or delete a namespace that lacks previewd's managed-by and instance labels — protects against squatting and against naming collisions with existing namespaces — a legitimately pre-created namespace cannot be used; the PR shows a `failure` status explaining why.
- Ruling: server-side apply with field manager `previewd` and `force` — declarative, conflict-aware, and a no-op apply does not bump resourceVersion (verified by `TestReconcileIsIdempotent`) — forcing ownership overrides manual edits to fields previewd manages; that is intended for disposable environments.
- Ruling: prune by listing each allowed kind with the instance label rather than keeping an inventory in status — survives lost status and needs no extra storage — costs one LIST per allowed kind per apply (9 with the default allowlist).
- Ruling: the expiry label is applied by a second field manager, outside the bundle and its hash — otherwise every PR activity (label, title edit) re-applies the bundle and flips the commit status to pending — the ConfigMap no longer carries the expiry time; templates cannot reference it.
- Ruling: readiness = every Deployment/StatefulSet in the bundle has rolled out (observedGeneration current, updated and available replicas ≥ desired) — mirrors `kubectl rollout status` and covers the common preview shapes — Jobs, custom resources and external dependencies are not health-checked.
- Ruling: a failed render leaves the previously applied bundle running and marks the environment Failed with exponential backoff (5s·2ⁿ, max 5m) — a template typo should not tear down a working preview — reviewers may look at a stale preview while the PR shows `failure`.
- Ruling: a failed apply posts `failure` instead of the `pending` it was about to post — the SCM should never see "pending" for an attempt that already failed — none significant.
- Ruling: read the PreviewEnvironment and the namespace through the uncached API reader; read workloads from the label-filtered cache — a stale cached PE caused duplicate status posts in the end-to-end test; workload staleness only delays readiness by one event — two extra GETs per reconcile.
- Ruling: SCM status posting is at-least-once — `lastReported` is written after a successful post; if the status patch then fails, the next reconcile posts again — GitHub statuses are idempotent from the reviewer's perspective (same state, same context); the cost is an occasional duplicate API call.
- Ruling: map previewd states onto GitHub commit statuses (pending/success/failure/error) rather than the Deployments API — commit statuses need only `statuses:write`, render on every PR, and are what branch protection understands — no deployment history or environment URL panel in GitHub's UI; the URL is in `target_url`.
- Ruling: `expired` maps to GitHub `error`, `inactive` (closed / label removed) to `success` — an expired preview is actionable ("push to recreate"); a deliberately removed one is not a failure of the change — teams that make `previewd` a required check will see a red check on idle PRs.

## Rendering and security

- Ruling: Go `text/template` with a small function map instead of Helm/Kustomize/sprig — keeps the rendering surface auditable and dependency-free, and makes the injection analysis tractable — teams with existing Helm charts must port them (see roadmap: Argo CD Application output).
- Ruling: guard rails (quota, limits, network policies, PSA labels) are generated from template-set config, not left to manifests — every preview gets them even if a template author forgets — less flexibility for unusual network layouts (config knobs cover ingress namespaces, egress lockdown and CIDRs).
- Ruling: RBAC and cluster-scoped kinds can never be allowlisted; ResourceQuota/LimitRange/NetworkPolicy cannot be templated — these are previewd's own guard rails and privilege boundaries — templates cannot add extra network policies; config knobs must be used.
- Ruling: Services are ClusterIP-only unless a template set allows more — previews should be reached through the shared ingress, and LoadBalancers cost money per PR — NodePort/LoadBalancer previews need an explicit opt-in.
- Ruling: fork PRs are ignored by default — forks run code from outside the organisation with whatever access the preview namespace has — fork-based contribution workflows must opt in with `--allow-forks`.
- Ruling: webhook secrets must be ≥ 16 characters and come from env or files, never flags — short secrets are brute-forceable offline from a captured delivery; flags leak via process listings — none.
- Ruling: delivery-ID dedupe is an in-memory LRU per replica, not shared state — correctness comes from the idempotent intake and `updated_at` ordering; the cache only saves API calls — after a restart or on another replica a duplicate costs one GET.

## Engineering

- Ruling: Go 1.26 module (toolchain 1.27 locally), controller-runtime v0.25.2, k8s.io v0.37.1, envtest assets 1.37.0 — the versions available offline in the module cache and envtest store on the build machine — older clusters (< 1.33) are untested.
- Ruling: `setup-envtest` and `controller-gen` are `tool` dependencies in go.mod — versions are pinned by go.sum and `make` works offline once the module cache is warm — go.mod lists their transitive dependencies.
- Ruling: `make check` requires envtest (PREVIEWD_REQUIRE_ENVTEST=1) and runs tests with `-race` — the controller is the core of the project; silently skipping its integration tests would make the gate meaningless — a machine without network or cached envtest binaries cannot run `make check` (plain `go test ./...` still runs everything else).
- Ruling: coverage is measured across `./internal/...` with `-coverpkg` in one run, floor 80% — `internal/` is all the product logic; `cmd/` is thin `main` plus the demo program — the demo's code is exercised by `make demo`, not counted.
- Ruling: the offline demo runs a real kube-apiserver + etcd (envtest) plus a small simulator for the deployment and namespace controllers, and a fake GitHub API — demonstrates the real controller over real HTTP without Docker or network — "Ready" in the demo means the simulator marked Deployments available; no containers run.
- Ruling: the demo uses a fake clock and fast-forwards it to show TTL expiry and retention — waiting 72h is not a demo — the time-to-ready histogram in the demo output is in simulated time.
- Ruling: deploy manifests reference image `previewd:dev` and no Dockerfile is shipped — no image has been built or published, and a Dockerfile that was never built would be an untested claim — users build their own image.

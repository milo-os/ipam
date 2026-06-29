# Test: `pool-list`

LIST coverage for IPPool (cluster-scoped) — regression guard for issue #54,
where `kubectl get ippools` returned an empty list even though the pools
existed and were fetchable by name.

Steps:
1. Create three IPPools → wait Ready.
2. Assert a cache-served LIST (`kubectl get ippools`, no resourceVersion →
   served from the apiserver watch cache) returns all three by name.
3. Assert the LIST carries a non-zero metadata.resourceVersion (the bug
   collapsed the list RV to 0/1, which leaves the watch cache unsynced).
4. Cross-check: each pool is also fetchable by name (the symptom was that
   by-name GET worked while LIST was empty) and the by-name set matches
   the LIST set.

NOTE: the underlying bug only manifests on a long-lived database whose
changelog has been pruned past its retention window (the steady state on
staging), so a fresh-cluster chainsaw run cannot reproduce the empty-list
failure on its own — the deterministic detector is the Go integration test
in internal/storage/postgres (TestGetList_ResourceVersionAfterChangelogPruned,
TestCurrentResourceVersion_AnchoredOnObjects), which prunes the changelog
explicitly. This suite is the end-to-end coverage guard for the LIST path.


## Steps

| # | Name | Bindings | Try | Catch | Finally | Cleanup |
|:-:|---|:-:|:-:|:-:|:-:|:-:|
| 1 | [create-pools](#step-create-pools) | 0 | 4 | 0 | 0 | 0 |
| 2 | [list-returns-pools](#step-list-returns-pools) | 0 | 1 | 0 | 0 | 0 |
| 3 | [by-name-matches-list](#step-by-name-matches-list) | 0 | 1 | 0 | 1 | 0 |

### Step: `create-pools`

Create three IPPools and wait for each to reach Ready.

#### Try

| # | Operation | Bindings | Outputs | Description |
|:-:|---|:-:|:-:|---|
| 1 | `create` | 0 | 0 | *No description* |
| 2 | `create` | 0 | 0 | *No description* |
| 3 | `create` | 0 | 0 | *No description* |
| 4 | `script` | 0 | 0 | *No description* |

### Step: `list-returns-pools`

A cache-served LIST must return all three pools and a non-zero
resourceVersion. This is the core #54 assertion: by-name GETs worked
while the LIST came back empty.


#### Try

| # | Operation | Bindings | Outputs | Description |
|:-:|---|:-:|:-:|---|
| 1 | `script` | 0 | 0 | *No description* |

### Step: `by-name-matches-list`

Each pool is fetchable by name and the by-name set equals the LIST set
— i.e. the cache is not silently hiding objects that exist.


#### Try

| # | Operation | Bindings | Outputs | Description |
|:-:|---|:-:|:-:|---|
| 1 | `script` | 0 | 0 | *No description* |

#### Finally

| # | Operation | Bindings | Outputs | Description |
|:-:|---|:-:|:-:|---|
| 1 | `script` | 0 | 0 | *No description* |

---


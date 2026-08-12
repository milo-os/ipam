#!/usr/bin/env bash
# Refuse overlapping root-pool ranges across every fixture suite.
#
# A root IPPool that overlaps another root of the same tenant is refused at
# create time. The e2e suites all run in one project and chainsaw runs them
# concurrently, so two suites holding overlapping roots is a create-time
# conflict — and an intermittent one, because it depends on which suite gets
# there first. The load fixtures share that project too.
#
# The unit of comparison is the SUITE, not the file: one suite naturally repeats
# its own range across a manifest, an assertion, and a script, while two suites
# sharing a range is the collision this exists to catch. Comparing files would
# report the former; comparing whole trees would miss the latter.
#
# The population is what the suites DECLARE, not what the cluster holds. A
# fixture that does not exist yet still collides the moment its suite runs, and
# the e2e roots are created at test time, so a range checked against live rows
# is checked against the wrong population. --live adds the cluster's undeclared
# roots as a courtesy on top.
#
# Usage:
#   hack/verify-fixture-ranges.sh            # declared fixtures only (CI)
#   hack/verify-fixture-ranges.sh --live     # also compare against the cluster
set -euo pipefail

cd "$(dirname "$0")/.."

LIVE=""
if [ "${1:-}" = "--live" ]; then
  LIVE="$(kubectl get ippools.ipam.miloapis.com -o json 2>/dev/null \
    | python3 -c '
import json,sys
try: d=json.load(sys.stdin)
except Exception: sys.exit(0)
for i in d.get("items", []):
    if not i.get("spec", {}).get("parentPoolRef"):
        c = i.get("spec", {}).get("cidr")
        if c: print(c)
' || true)"
fi

LIVE="$LIVE" python3 - <<'PY'
import ipaddress, os, re, pathlib, sys

# Fallback for shell and JavaScript fixtures, which have no YAML structure.
PAT = re.compile(r"""(?:cidr|CIDR|ROOT_CIDR)['"]?\s*[:=]\s*['"]?([0-9a-fA-F:.]+/\d{1,3})""")

def yaml_roots(text):
    """Root-pool CIDRs in a YAML file: IPPool documents with no parentPoolRef.

    A child pool takes its range from its parent and nests by construction, so
    including one would report every hierarchy fixture as an overlap."""
    out = []
    for doc in text.split("\n---"):
        if "kind: IPPool" not in doc or "parentPoolRef" in doc:
            continue
        m = re.search(r"^\s*cidr:\s*['\"]?([0-9a-fA-F:.]+/\d{1,3})", doc, re.M)
        if m:
            out.append(m.group(1))
    return out

def suite_of(path):
    """The owning suite. Everything under test/load is one suite: those scripts
    share a fixture set by design and verify themselves internally."""
    parts = path.parts
    if len(parts) >= 2 and parts[0] == "test" and parts[1] == "load":
        return "load"
    if len(parts) >= 3 and parts[0] == "test" and parts[1] == "e2e":
        return f"e2e/{parts[2]}"
    return "/".join(parts[:2])

def collect():
    suites = {}
    for root, suffixes in (("test/e2e", {".yaml", ".yml", ".sh"}),
                           ("test/load", {".js", ".yaml", ".yml", ".sh"})):
        base = pathlib.Path(root)
        if not base.exists():
            continue
        for p in base.rglob("*"):
            if not p.is_file() or p.suffix not in suffixes:
                continue
            text = p.read_text(errors="ignore")
            found = yaml_roots(text) if p.suffix in {".yaml", ".yml"} else \
                    [m.group(1) for m in PAT.finditer(text)]
            for c in found:
                try:
                    n = ipaddress.ip_network(c)
                except ValueError:
                    continue
                # A single-address literal is a claim or an assertion, not a
                # root pool. Including them trains people to ignore this check.
                if n.prefixlen == n.max_prefixlen:
                    continue
                suites.setdefault(suite_of(p), {}).setdefault(c, set()).add(str(p))
    return suites

suites = collect()

declared = {c for s in suites.values() for c in s}
live_extra = [c for c in (l.strip() for l in os.environ.get("LIVE", "").splitlines())
              if c and c not in declared]
if live_extra:
    suites["cluster (undeclared)"] = {c: {"cluster"} for c in live_extra}

total = sum(len(v) for v in suites.values())
for name in sorted(suites):
    print(f"  {name:<34} {len(suites[name])} root range(s)")
print(f"  {'TOTAL':<34} {total}")

problems = []
names = sorted(suites)
for i, a in enumerate(names):
    for b in names[i + 1:]:
        for ca, wa in sorted(suites[a].items()):
            na = ipaddress.ip_network(ca)
            for cb, wb in sorted(suites[b].items()):
                nb = ipaddress.ip_network(cb)
                if na.version == nb.version and na.overlaps(nb):
                    problems.append((a, ca, sorted(wa), b, cb, sorted(wb)))

if problems:
    print()
    print(f"FAIL: {len(problems)} overlapping root range(s) between fixture suites.")
    print()
    print("The suites share one project and run concurrently, so whichever suite")
    print("creates its root second is refused. Give each suite its own range.")
    print()
    for a, ca, wa, b, cb, wb in problems:
        print(f"  {a} {ca}")
        for w in wa:
            print(f"      {w}")
        print(f"  overlaps {b} {cb}")
        for w in wb:
            print(f"      {w}")
        print()
    sys.exit(1)

print("verify-fixture-ranges: OK — every suite's root ranges are disjoint")
PY

// Package fixtures guards properties of the e2e and load fixture sets that no
// single suite can check about itself.
package fixtures

import (
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// repoRoot is relative to this package: go test runs with the package directory
// as the working directory.
const repoRoot = "../.."

// extraRootCIDRsEnv carries root ranges that exist in a cluster but no fixture
// declares. hack/verify-fixture-ranges.sh --live fills it from kubectl.
const extraRootCIDRsEnv = "IPAM_EXTRA_ROOT_CIDRS"

// clusterSuite labels ranges that come from a cluster rather than a fixture.
const clusterSuite = "cluster (undeclared)"

var (
	// cidrField matches the cidr field of a YAML IPPool document.
	cidrField = regexp.MustCompile(`(?m)^\s*cidr:\s*['"]?([0-9a-fA-F:.]+/\d{1,3})`)

	// cidrLiteral is the fallback for shell and JavaScript fixtures, which have
	// no YAML structure to read.
	cidrLiteral = regexp.MustCompile(`(?:cidr|CIDR|ROOT_CIDR)['"]?\s*[:=]\s*['"]?([0-9a-fA-F:.]+/\d{1,3})`)
)

// fixtureTrees are the roots to scan and the file types that can declare a
// range in each.
var fixtureTrees = []struct {
	dir      string
	suffixes []string
}{
	{"test/e2e", []string{".yaml", ".yml", ".sh"}},
	{"test/load", []string{".js", ".yaml", ".yml", ".sh"}},
}

// TestFixtureRootRangesAreDisjoint refuses overlapping root-pool ranges across
// fixture suites.
//
// A root IPPool that overlaps another root of the same tenant is refused at
// create time. The e2e suites all run in one project and chainsaw runs them
// concurrently, so two suites holding overlapping roots is a create-time
// conflict — and an intermittent one, because it depends on which suite gets
// there first. The load fixtures share that project too.
//
// The unit of comparison is the suite, not the file: one suite naturally
// repeats its own range across a manifest, an assertion, and a script, while
// two suites sharing a range is the collision this exists to catch. Comparing
// files would report the former; comparing whole trees would miss the latter.
//
// The population is what the suites declare, not what a cluster holds. A
// fixture that does not exist yet still collides the moment its suite runs, and
// the e2e roots are created at test time, so a range checked against live rows
// is checked against the wrong population.
func TestFixtureRootRangesAreDisjoint(t *testing.T) {
	suites, err := collectRootRanges()
	if err != nil {
		t.Fatalf("collect fixture ranges: %v", err)
	}
	addExtraRootCIDRs(suites, os.Getenv(extraRootCIDRsEnv))

	if len(suites) == 0 {
		t.Fatal("no fixture ranges found: the scan is looking in the wrong place")
	}
	logInventory(t, suites)

	for _, overlap := range findOverlaps(t, suites) {
		t.Errorf("%s and %s both hold %s / %s\n"+
			"  %s\n    %s\n  %s\n    %s\n"+
			"The suites share one project and chainsaw runs them concurrently, so "+
			"whichever creates its root second is refused. Give each suite its own range.",
			overlap.suiteA, overlap.suiteB, overlap.cidrA, overlap.cidrB,
			overlap.cidrA, strings.Join(overlap.filesA, "\n    "),
			overlap.cidrB, strings.Join(overlap.filesB, "\n    "))
	}
}

// rootRanges maps a suite name to its root CIDRs, and each CIDR to the files
// declaring it.
type rootRanges map[string]map[string]map[string]bool

// collectRootRanges reads every fixture file and indexes the root ranges it
// declares by owning suite.
func collectRootRanges() (rootRanges, error) {
	suites := rootRanges{}
	for _, tree := range fixtureTrees {
		base := filepath.Join(repoRoot, tree.dir)
		if _, err := os.Stat(base); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(base, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !hasSuffix(path, tree.suffixes) {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel := filepath.ToSlash(strings.TrimPrefix(path, repoRoot+string(filepath.Separator)))
			for _, cidr := range declaredRoots(path, string(data)) {
				prefix, parseErr := netip.ParsePrefix(cidr)
				if parseErr != nil {
					continue
				}
				// A single-address literal is a claim or an assertion, not a
				// root pool. Including them trains people to ignore this check.
				if prefix.Bits() == prefix.Addr().BitLen() {
					continue
				}
				suite := suiteOf(rel)
				if suites[suite] == nil {
					suites[suite] = map[string]map[string]bool{}
				}
				if suites[suite][cidr] == nil {
					suites[suite][cidr] = map[string]bool{}
				}
				suites[suite][cidr][rel] = true
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return suites, nil
}

// declaredRoots returns the root CIDRs one file declares: structurally for
// YAML, by pattern for everything else.
func declaredRoots(path, text string) []string {
	if hasSuffix(path, []string{".yaml", ".yml"}) {
		return yamlRoots(text)
	}
	var roots []string
	for _, match := range cidrLiteral.FindAllStringSubmatch(text, -1) {
		roots = append(roots, match[1])
	}
	return roots
}

// yamlRoots returns the CIDRs of IPPool documents that name no parent.
//
// A child pool takes its range from its parent and nests by construction, so
// including one would report every hierarchy fixture as an overlap.
func yamlRoots(text string) []string {
	var roots []string
	for _, doc := range strings.Split(text, "\n---") {
		if !strings.Contains(doc, "kind: IPPool") || strings.Contains(doc, "parentPoolRef") {
			continue
		}
		if match := cidrField.FindStringSubmatch(doc); match != nil {
			roots = append(roots, match[1])
		}
	}
	return roots
}

// suiteOf returns the suite owning a fixture path. Everything under test/load
// is one suite: those scripts share a fixture set by design and verify
// themselves internally.
func suiteOf(rel string) string {
	parts := strings.Split(rel, "/")
	if len(parts) >= 2 && parts[0] == "test" && parts[1] == "load" {
		return "load"
	}
	if len(parts) >= 3 && parts[0] == "test" && parts[1] == "e2e" {
		return "e2e/" + parts[2]
	}
	if len(parts) >= 2 {
		return strings.Join(parts[:2], "/")
	}
	return rel
}

// addExtraRootCIDRs folds cluster ranges no fixture declares into their own
// pseudo-suite.
func addExtraRootCIDRs(suites rootRanges, raw string) {
	declared := map[string]bool{}
	for _, ranges := range suites {
		for cidr := range ranges {
			declared[cidr] = true
		}
	}
	for _, cidr := range strings.Fields(raw) {
		if declared[cidr] {
			continue
		}
		if _, err := netip.ParsePrefix(cidr); err != nil {
			continue
		}
		if suites[clusterSuite] == nil {
			suites[clusterSuite] = map[string]map[string]bool{}
		}
		suites[clusterSuite][cidr] = map[string]bool{"cluster": true}
	}
}

type overlap struct {
	suiteA, cidrA string
	filesA        []string
	suiteB, cidrB string
	filesB        []string
}

// findOverlaps returns every pair of ranges that overlap across two suites.
func findOverlaps(t *testing.T, suites rootRanges) []overlap {
	t.Helper()
	var overlaps []overlap
	names := sortedKeys(suites)
	for i, first := range names {
		for _, second := range names[i+1:] {
			for _, cidrA := range sortedKeys(suites[first]) {
				a := mustParse(t, cidrA)
				for _, cidrB := range sortedKeys(suites[second]) {
					b := mustParse(t, cidrB)
					if !prefixesOverlap(a, b) {
						continue
					}
					overlaps = append(overlaps, overlap{
						suiteA: first, cidrA: cidrA, filesA: sortedKeys(suites[first][cidrA]),
						suiteB: second, cidrB: cidrB, filesB: sortedKeys(suites[second][cidrB]),
					})
				}
			}
		}
	}
	return overlaps
}

// prefixesOverlap reports whether either prefix contains the other's base
// address. Same-family only: an IPv4 and an IPv6 range never collide.
func prefixesOverlap(a, b netip.Prefix) bool {
	if a.Addr().Is4() != b.Addr().Is4() {
		return false
	}
	return a.Contains(b.Addr()) || b.Contains(a.Addr())
}

func logInventory(t *testing.T, suites rootRanges) {
	t.Helper()
	total := 0
	for _, name := range sortedKeys(suites) {
		t.Logf("  %-34s %d root range(s)", name, len(suites[name]))
		total += len(suites[name])
	}
	t.Logf("  %-34s %d", "TOTAL", total)
}

func mustParse(t *testing.T, cidr string) netip.Prefix {
	t.Helper()
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		t.Fatalf("parse %q: %v", cidr, err)
	}
	return prefix
}

func hasSuffix(path string, suffixes []string) bool {
	ext := filepath.Ext(path)
	for _, suffix := range suffixes {
		if ext == suffix {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

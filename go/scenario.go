// The seeded scenario, ported from the kit's program/env/src/wire.ts.
// The suite's checks name the viewer/stranger logins, repo names and branch
// names from this one deterministic generator (seed 0x6a09e667f3bcc909).
// The entry's bearer token is opaque, so the only way to know which login
// the token names is to recompute the scenario exactly as the suite does.
// Every stream is seeded by its label, so only the streams we need are
// walked; the values must match the kit byte for byte (verified by test).
package main

const scenarioSeed uint64 = 0x6a09e667f3bcc909

var firstNames = []string{
	"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo", "iris", "jonas", "kira", "lars",
	"mira", "nico", "olga", "pavel", "quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane",
}

var lastNames = []string{
	"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia", "holm", "iversen", "jansen", "keller",
	"lind", "moreau", "novak", "olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega", "weber",
	"xu", "yilmaz", "zimmer",
}

var repoAdjectives = []string{
	"amber", "brisk", "calm", "crisp", "eager", "faint", "gentle", "happy", "idle", "jolly", "keen", "lively",
	"mellow", "nimble", "open", "plain", "quick", "quiet", "rapid", "soft", "steady", "tidy", "upbeat",
	"vivid", "warm", "witty", "young", "zesty",
}

var repoNouns = []string{
	"acorn", "badger", "canyon", "cedar", "comet", "coral", "delta", "ember", "falcon", "glade", "harbor",
	"indigo", "juniper", "kettle", "lantern", "meadow", "nebula", "orchid", "pebble", "quartz", "raven",
	"saffron", "thistle", "umber", "violet", "willow", "yarrow", "zephyr",
}

var repoDescriptions = []string{
	"a small service with a sharp edge",
	"notes and experiments, kept until they are not",
	"the tooling behind the thing",
	"a reference implementation, for reading",
	"what the prototype became",
	"fixtures, generators and the odd benchmark",
	"the parts that must not break",
	"a sandbox for the next idea",
}

var pullSubjects = []string{
	"shallow fetch support", "push report status checks", "issue label validation", "merge conflict detection",
	"GraphQL cursor pagination", "MCP tool schemas", "packfile delta reuse", "conditional requests",
	"repo pagination", "branch protection stubs",
}

var issueLabels = []string{"bug", "enhancement", "documentation", "question", "good first issue", "help wanted"}

// ---------------------------------------------------------------- PRNG

func fnv1a64(text string) uint64 {
	hash := uint64(0xcbf29ce484222325)
	for i := 0; i < len(text); i++ {
		hash ^= uint64(text[i])
		hash *= 0x100000001b3
	}
	return hash
}

func mix64(v uint64) uint64 {
	z := v + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func scenarioSeedFor(label string) uint64 {
	return mix64(scenarioSeed ^ fnv1a64(label))
}

// rng is xorshift64*, the kit's Rng. uint64 arithmetic wraps exactly like
// the kit's 64-bit masked BigInt arithmetic.
type rng struct{ state uint64 }

func scenarioRng(label string) *rng {
	s := scenarioSeedFor(label)
	if s == 0 {
		s = scenarioSeed
	}
	return &rng{state: s}
}

func (r *rng) nextU64() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 0x2545f4914f6cdd1d
}

func (r *rng) nextU32() uint32 {
	return uint32(r.nextU64() >> 32)
}

func (r *rng) nextInt(maxExclusive int) int {
	if maxExclusive <= 1 {
		return 0
	}
	const range32 = uint64(4294967296)
	limit := range32 - (range32 % uint64(maxExclusive))
	for {
		v := uint64(r.nextU32())
		if v < limit {
			return int(v % uint64(maxExclusive))
		}
	}
}

func (r *rng) fraction() float64 {
	return float64(r.nextU32()) / 4294967296
}

func (r *rng) chance(p float64) bool { return r.fraction() < p }

func (r *rng) pick(items []string) string {
	return items[r.nextInt(len(items))]
}

// ---------------------------------------------------------------- scenario

type scenUser struct {
	id    int
	login string
	name  string
	email string
}

type scenRepo struct {
	id            int
	owner         string
	name          string
	description   string
	private       bool
	defaultBranch string
}

type scenario struct {
	viewer        scenUser
	teammate      scenUser
	stranger      scenUser
	users         []scenUser
	org           string
	repos         []scenRepo // viewer repos, ids 5000+
	orgRepos      []scenRepo
	strangerRepos []scenRepo
	labels        []string
	branches      []string
}

func generateUsers(r *rng, count int) []scenUser {
	used := map[string]bool{}
	users := make([]scenUser, 0, count)
	for i := 0; i < count; i++ {
		first := r.pick(firstNames)
		last := r.pick(lastNames)
		login := first + last
		suffix := 2
		for used[login] {
			login = first + last + "-" + itoa(suffix)
			suffix++
		}
		used[login] = true
		users = append(users, scenUser{id: 1000 + i, login: login, name: first + " " + last, email: login + "@example.invalid"})
	}
	return users
}

func generateRepos(r *rng, owner string, count int, firstID int) []scenRepo {
	used := map[string]bool{}
	repos := make([]scenRepo, 0, count)
	for i := 0; i < count; i++ {
		base := r.pick(repoAdjectives) + "-" + r.pick(repoNouns)
		name := base
		suffix := 2
		for used[name] {
			name = base + "-" + itoa(suffix)
			suffix++
		}
		used[name] = true
		repos = append(repos, scenRepo{
			id: firstID + i, owner: owner, name: name,
			description: r.pick(repoDescriptions),
			private:       i%7 == 3,
			defaultBranch: "main",
		})
	}
	return repos
}

func generateBranchName(r *rng) string {
	kind := r.pick([]string{"feature", "fix", "perf", "docs", "chore"})
	subject := r.pick(pullSubjects)
	subject = lowerASCII(subject)
	subject = slugify(subject)
	return kind + "/" + subject
}

func uniqueBranch(r *rng, used map[string]bool) string {
	branch := generateBranchName(r)
	suffix := 2
	for used[branch] {
		branch = generateBranchName(r) + "-" + itoa(suffix)
		suffix++
	}
	used[branch] = true
	return branch
}

func buildScenario() *scenario {
	users := generateUsers(scenarioRng("users"), 12)
	viewer := users[0]
	teammate := users[1]
	stranger := users[2]
	orgRng := scenarioRng("org")
	org := orgRng.pick(repoAdjectives) + "-" + orgRng.pick(repoNouns) + "-org"
	branchRng := scenarioRng("branches")
	branchSet := map[string]bool{"main": true, "develop": true, "release/1.0": true}
	branches := []string{"main", "develop", "release/1.0"}
	for i := 0; i < 8; i++ {
		branches = append(branches, uniqueBranch(branchRng, branchSet))
	}
	return &scenario{
		viewer:        viewer,
		teammate:      teammate,
		stranger:      stranger,
		users:         users,
		org:           org,
		repos:         generateRepos(scenarioRng("repos.viewer"), viewer.login, 30, 5000),
		orgRepos:      generateRepos(scenarioRng("repos.org"), org, 6, 6000),
		strangerRepos: generateRepos(scenarioRng("repos.stranger"), stranger.login, 3, 7000),
		labels:        issueLabels,
		branches:      branches,
	}
}

// slugify lowercases ASCII and replaces runs of non [a-z0-9] with '-',
// trimming leading/trailing '-'. Mirrors the kit's JS replace chain.
func slugify(s string) string {
	var b []byte
	lastDash := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b = append(b, c)
			lastDash = false
		} else if !lastDash {
			b = append(b, '-')
			lastDash = true
		}
	}
	// trim leading/trailing '-'
	for len(b) > 0 && b[0] == '-' {
		b = b[1:]
	}
	for len(b) > 0 && b[len(b)-1] == '-' {
		b = b[:len(b)-1]
	}
	return string(b)
}

func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

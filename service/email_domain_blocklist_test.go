package service

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func withBlocklistState(t *testing.T, domains []string) {
	t.Helper()
	disposableEmailMu.Lock()
	previous := disposableEmailDomains
	disposableEmailDomains = make(map[string]struct{}, len(domains))
	for _, domain := range domains {
		disposableEmailDomains[domain] = struct{}{}
	}
	disposableEmailMu.Unlock()

	previousExtra := common.DisposableEmailExtraDomains
	previousAllow := common.DisposableEmailAllowlist
	previousMode := common.DisposableEmailHeuristicMode
	previousEnabled := common.DisposableEmailBlockEnabled
	previousPrune := disposableEmailLastPrune
	common.DisposableEmailBlockEnabled = true
	t.Cleanup(func() {
		disposableEmailMu.Lock()
		disposableEmailDomains = previous
		disposableEmailMu.Unlock()
		common.DisposableEmailExtraDomains = previousExtra
		common.DisposableEmailAllowlist = previousAllow
		common.DisposableEmailHeuristicMode = previousMode
		common.DisposableEmailBlockEnabled = previousEnabled
		disposableEmailAutoMu.Lock()
		disposableEmailAutoBlocked = make(map[string]int64)
		disposableEmailTrackers = make(map[string]*sourceDomainRecord)
		disposableEmailLastPrune = previousPrune
		disposableEmailAutoMu.Unlock()
	})
}

func TestCheckDisposableEmailMatchesParentDomains(t *testing.T) {
	withBlocklistState(t, []string{"886211.xyz", "10mail.org", "mailinator.com"})
	common.DisposableEmailHeuristicMode = DisposableEmailHeuristicOff

	cases := []struct {
		email   string
		blocked bool
	}{
		{"user@886211.xyz", true},
		{"user@860yg0yi.886211.xyz", true},
		{"user@deep.sub.886211.xyz", true},
		{"user@886211.xyz.evil.com", false},
		{"user@mailinator.com", true},
		{"USER@Mailinator.COM", true},
		{"user@notmailinator.com", false},
		{"user@qq.com", false},
		{"user@company.xyz", false},
		{"", false},
		{"no-at-sign", false},
	}
	for _, tc := range cases {
		err := CheckDisposableEmail(t.Context(), tc.email, "")
		if tc.blocked {
			assert.ErrorIs(t, err, ErrDisposableEmailRejected, tc.email)
			continue
		}
		assert.NoError(t, err, tc.email)
	}
}

func TestCheckDisposableEmailOperatorLists(t *testing.T) {
	withBlocklistState(t, nil)
	common.DisposableEmailHeuristicMode = DisposableEmailHeuristicOff
	common.DisposableEmailExtraDomains = []string{"smailr.com", "temp.example"}

	assert.ErrorIs(t, CheckDisposableEmail(t.Context(), "a@smailr.com", ""), ErrDisposableEmailRejected)
	assert.ErrorIs(t, CheckDisposableEmail(t.Context(), "a@sub.temp.example", ""), ErrDisposableEmailRejected)

	// The allowlist wins over both the operator supplement and the synced list.
	common.DisposableEmailAllowlist = []string{"smailr.com"}
	assert.NoError(t, CheckDisposableEmail(t.Context(), "a@smailr.com", ""))
	assert.ErrorIs(t, CheckDisposableEmail(t.Context(), "a@sub.temp.example", ""), ErrDisposableEmailRejected)
}

func TestCheckDisposableEmailAllowlistOverridesSyncedList(t *testing.T) {
	withBlocklistState(t, []string{"mailinator.com"})
	common.DisposableEmailHeuristicMode = DisposableEmailHeuristicOff
	common.DisposableEmailAllowlist = []string{"mailinator.com"}

	assert.NoError(t, CheckDisposableEmail(t.Context(), "a@mailinator.com", ""))
}

func TestDisposableEmailHeuristicGeneratedLabels(t *testing.T) {
	withBlocklistState(t, nil)
	common.DisposableEmailHeuristicMode = DisposableEmailHeuristicOff

	generated := []string{
		"886211.xyz",
		"860yg0yi.886211.xyz",
		"a1b2c3d4.top",
		"123456789.shop",
	}
	for _, domain := range generated {
		reason, hit := randomLabelReason(domain)
		assert.True(t, hit, domain)
		assert.NotEmpty(t, reason, domain)
	}

	ordinary := []string{
		"qq.com",
		"company.xyz",
		"shop2024.com",
		"gmail.com",
		"myblog.top",
		"abc.xyz",
	}
	for _, domain := range ordinary {
		_, hit := randomLabelReason(domain)
		assert.False(t, hit, domain)
	}
}

func TestDisposableEmailHeuristicModes(t *testing.T) {
	withBlocklistState(t, nil)

	// Log mode accepts and only records.
	common.DisposableEmailHeuristicMode = DisposableEmailHeuristicLog
	assert.NoError(t, CheckDisposableEmail(t.Context(), "a@886211.xyz", ""))

	common.DisposableEmailHeuristicMode = DisposableEmailHeuristicBlock
	assert.ErrorIs(t, CheckDisposableEmail(t.Context(), "a@886211.xyz", ""), ErrDisposableEmailRejected)

	common.DisposableEmailHeuristicMode = DisposableEmailHeuristicOff
	assert.NoError(t, CheckDisposableEmail(t.Context(), "a@886211.xyz", ""))
}

func TestDisposableEmailSourceSpreadAcrossSubdomains(t *testing.T) {
	withBlocklistState(t, nil)
	common.DisposableEmailHeuristicMode = DisposableEmailHeuristicBlock

	// mail.xyz is not itself a generated domain, but it sits under a
	// low-reputation TLD, so subdomain spread behind one source is tracked.
	const ip = "203.0.113.9"
	assert.NoError(t, CheckDisposableEmail(t.Context(), "a@one.mail.xyz", ip))
	assert.NoError(t, CheckDisposableEmail(t.Context(), "b@two.mail.xyz", ip))
	// The third distinct subdomain of the same parent trips the rule.
	assert.ErrorIs(t, CheckDisposableEmail(t.Context(), "c@three.mail.xyz", ip), ErrDisposableEmailRejected)
	// A different source is unaffected.
	assert.NoError(t, CheckDisposableEmail(t.Context(), "d@four.mail.xyz", "203.0.113.10"))
	// A single repeated domain does not.
	for range 3 {
		assert.NoError(t, CheckDisposableEmail(t.Context(), "e@same.mail.xyz", "203.0.113.11"))
	}
}

func TestSyncDisposableEmailBlocklistKeepsPreviousOnShortDownload(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("EMAIL_BLOCKLIST_DIR", dir)
	path := filepath.Join(dir, disposableEmailBlocklistFile)
	previousFile := "# synced\nmailinator.com\n"
	require.NoError(t, os.WriteFile(path, []byte(previousFile), 0644))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "only.example\n")
	}))
	defer server.Close()
	previousSource := disposableEmailSyncSource
	disposableEmailSyncSource = server.URL
	t.Cleanup(func() { disposableEmailSyncSource = previousSource })

	_, err := SyncDisposableEmailBlocklist(t.Context())
	require.Error(t, err)
	onDisk, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, previousFile, string(onDisk))

	domains, err := parseDisposableEmailBlocklist(strings.NewReader(`
# comment
Mailinator.COM
* . skipped
886211.xyz
0-mail.com
temp-mail.io
bad entry
`))
	require.NoError(t, err)
	assert.Equal(t, map[string]struct{}{
		"mailinator.com": {},
		"886211.xyz":     {},
		"0-mail.com":     {},
		"temp-mail.io":   {},
	}, domains)
}

func TestLoadDisposableEmailBlocklistReadsFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("EMAIL_BLOCKLIST_DIR", dir)
	// Distinct filler domains: the loader counts unique entries, and a list
	// below the minimum is rejected on purpose.
	var content strings.Builder
	content.WriteString("# synced\n10mail.org\nmailinator.com\n")
	for i := range 1200 {
		fmt.Fprintf(&content, "filler%d.example\n", i)
	}
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, disposableEmailBlocklistFile),
		[]byte(content.String()),
		0644,
	))

	withBlocklistState(t, nil)
	require.NoError(t, LoadDisposableEmailBlocklist())
	t.Cleanup(func() {
		disposableEmailMu.Lock()
		disposableEmailDomains = make(map[string]struct{})
		disposableEmailFileEntries = 0
		disposableEmailMu.Unlock()
	})

	entries, _, _ := disposableEmailBlocklistState()
	assert.Equal(t, 1202, entries)
	assert.True(t, isBlockedEmailDomain("a.10mail.org"))
	assert.False(t, isBlockedEmailDomain("example.com"))
}

func TestLoadDisposableEmailBlocklistMissingFileIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("EMAIL_BLOCKLIST_DIR", dir)

	withBlocklistState(t, []string{"seed.example"})
	require.NoError(t, LoadDisposableEmailBlocklist())
	assert.False(t, isBlockedEmailDomain("seed.example"))
}

func TestLoadDisposableEmailBlocklistRejectsTruncatedFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("EMAIL_BLOCKLIST_DIR", dir)
	// A file that parses cleanly but holds almost nothing must not replace a
	// working list: an interrupted copy would otherwise disable the policy.
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, disposableEmailBlocklistFile),
		[]byte("mailinator.com\n"), 0644,
	))

	withBlocklistState(t, []string{"keep.example"})
	require.Error(t, LoadDisposableEmailBlocklist())
	assert.True(t, isBlockedEmailDomain("keep.example"))
}

func TestSourceSpreadIgnoresCorporateParentDomains(t *testing.T) {
	withBlocklistState(t, nil)
	common.DisposableEmailHeuristicMode = DisposableEmailHeuristicBlock

	const ip = "198.51.100.7"
	for _, email := range []string{"alice@corp.example", "bob@corp.example", "carol@eu.corp.example"} {
		assert.NoError(t, CheckDisposableEmail(t.Context(), email, ip), email)
	}
}

func TestSourceSpreadRespectsAllowlist(t *testing.T) {
	withBlocklistState(t, nil)
	common.DisposableEmailHeuristicMode = DisposableEmailHeuristicBlock
	common.DisposableEmailAllowlist = []string{"mail.xyz"}

	const ip = "198.51.100.8"
	for _, email := range []string{"a@one.mail.xyz", "b@two.mail.xyz", "c@three.mail.xyz"} {
		assert.NoError(t, CheckDisposableEmail(t.Context(), email, ip), email)
	}
}

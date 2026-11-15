package service

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
)

// Disposable email policy.
//
// Three layers, checked for every address in this order:
//  1. allowlist: operator override, wins over everything below
//  2. blocklist: the externally maintained list synced daily from
//     disposable-email-domains/disposable-email-domains, plus the operator's
//     own supplement (common.DisposableEmailExtraDomains)
//  3. heuristics for services no list keeps up with: random-looking labels
//     under low-reputation TLDs, and one registration source spreading across
//     several subdomains of the same parent domain
//
// Matching walks parent domains, so an entry for "886211.xyz" also matches
// "860yg0yi.886211.xyz". Bare TLDs are never matched, so no entry can shade an
// entire TLD.

const (
	// disposableEmailBlocklistURL is the upstream list refreshed by the daily
	// sync task (CC0, updated several times a week).
	disposableEmailBlocklistURL = "https://raw.githubusercontent.com/disposable-email-domains/disposable-email-domains/master/disposable_email_blocklist.conf"

	DisposableEmailHeuristicOff   = "off"
	DisposableEmailHeuristicLog   = "log"
	DisposableEmailHeuristicBlock = "block"

	disposableEmailBlocklistFile     = "disposable_email_blocklist.conf"
	disposableEmailBlocklistMinLines = 1000 // refuse a truncated download
	disposableEmailMaxFileBytes      = 8 << 20
	disposableEmailSyncTimeout       = 60 * time.Second

	// One source using this many distinct subdomains of the same parent domain
	// is treated as throwaway-address farming.
	disposableEmailParentDomainThreshold = 3
	disposableEmailParentDomainWindow    = 24 * time.Hour
	disposableEmailAutoBlockTTL          = 7 * 24 * time.Hour
	disposableEmailTrackerMaxEntries     = 20000
	disposableEmailPruneInterval         = 10 * time.Minute
)

// ErrDisposableEmailRejected is returned for addresses rejected by this policy.
var ErrDisposableEmailRejected = errors.New("Disposable email addresses are not accepted here. Please use a personal or company mailbox.")

var (
	disposableEmailMu          sync.RWMutex
	disposableEmailDomains     = make(map[string]struct{})
	disposableEmailLoadedAt    int64
	disposableEmailFileEntries int

	disposableEmailAutoMu      sync.Mutex
	disposableEmailAutoBlocked = make(map[string]int64) // parent domain -> expiry unix
	disposableEmailTrackers    = make(map[string]*sourceDomainRecord)
	disposableEmailLastPrune   = time.Now()
)

// sourceDomainRecord counts distinct full domains seen behind one source, per
// parent domain.
type sourceDomainRecord struct {
	Updated int64
	Parents map[string]map[string]int64 // parent -> full domain -> last seen
}

// lowReputationTLDs are TLDs where throwaway services cluster. They are only
// used together with a random-looking label, never on their own.
var lowReputationTLDs = map[string]struct{}{
	"xyz": {}, "top": {}, "shop": {}, "icu": {}, "cyou": {}, "buzz": {},
	"sbs": {}, "rest": {}, "click": {}, "link": {}, "loan": {}, "work": {},
	"men": {}, "pw": {}, "tk": {}, "ml": {}, "ga": {}, "cf": {}, "gq": {},
	"monster": {}, "quest": {}, "beauty": {}, "hair": {}, "skin": {}, "makeup": {},
	"cfd": {}, "lol": {}, "bond": {}, "autos": {}, "boats": {}, "homes": {},
}

// Labels may contain hyphens: real throwaway brands such as 0-mail.com and
// 10-minute-mail.com are in the upstream list, and an operator typing
// temp-mail.io into the supplement must not get a silent no-op.
var disposableEmailLabelPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// disposableEmailSyncSource is the URL the sync task fetches. Tests point it at
// a local server to exercise the truncated-download guard.
var disposableEmailSyncSource = disposableEmailBlocklistURL

// disposableEmailBlocklistPath is the on-disk list location. The container
// mounts its data volume at /data.
func disposableEmailBlocklistPath() string {
	dir := common.GetEnvOrDefaultString("EMAIL_BLOCKLIST_DIR", "/data")
	return filepath.Join(dir, disposableEmailBlocklistFile)
}

// LoadDisposableEmailBlocklist reads the on-disk list into memory. A missing
// file is not an error: the supplement and heuristics still work. A file that
// parses to fewer than disposableEmailBlocklistMinLines entries is rejected so
// a truncated copy cannot silently turn the policy off.
func LoadDisposableEmailBlocklist() error {
	path := disposableEmailBlocklistPath()
	file, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		replaceDisposableEmailDomains(nil)
		return nil
	}
	defer file.Close()

	domains, err := parseDisposableEmailBlocklist(io.LimitReader(file, disposableEmailMaxFileBytes))
	if err != nil {
		return err
	}
	if len(domains) < disposableEmailBlocklistMinLines {
		return fmt.Errorf("refusing to load blocklist %s: file holds only %d domains", path, len(domains))
	}
	replaceDisposableEmailDomains(domains)
	return nil
}

// replaceDisposableEmailDomains swaps the in-memory list under the write lock.
func replaceDisposableEmailDomains(domains map[string]struct{}) {
	disposableEmailMu.Lock()
	defer disposableEmailMu.Unlock()
	disposableEmailDomains = domains
	disposableEmailFileEntries = len(domains)
	if domains == nil {
		disposableEmailLoadedAt = 0
		return
	}
	disposableEmailLoadedAt = common.GetTimestamp()
}

func parseDisposableEmailBlocklist(r io.Reader) (map[string]struct{}, error) {
	domains := make(map[string]struct{}, 8192)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	dropped := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		domain := normalizeBlocklistDomain(line)
		if domain == "" {
			dropped++
			continue
		}
		domains[domain] = struct{}{}
	}
	if dropped > 0 {
		// A count that suddenly jumps means the upstream format changed and
		// entries are being silently discarded.
		logger.LogWarn(context.Background(), common.LogText("disposable email blocklist: discarded %d unparsable lines", dropped))
	}
	return domains, scanner.Err()
}

func normalizeBlocklistDomain(line string) string {
	line = strings.ToLower(strings.TrimSpace(line))
	if line == "" || strings.HasPrefix(line, "#") {
		return ""
	}
	line = strings.TrimPrefix(line, "*.")
	line = strings.TrimPrefix(line, "@")
	line = strings.Trim(line, ".")
	if !strings.Contains(line, ".") || strings.ContainsAny(line, " \t/") {
		return ""
	}
	for _, label := range strings.Split(line, ".") {
		if !disposableEmailLabelPattern.MatchString(label) {
			return ""
		}
	}
	return line
}

// disposableEmailBlocklistState reports the loaded list state for tests.
func disposableEmailBlocklistState() (entries int, loadedAt int64, path string) {
	disposableEmailMu.RLock()
	defer disposableEmailMu.RUnlock()
	return disposableEmailFileEntries, disposableEmailLoadedAt, disposableEmailBlocklistPath()
}

// SyncDisposableEmailBlocklist downloads the upstream list. A download that
// looks truncated keeps the previous file, and memory is reloaded only on
// success.
func SyncDisposableEmailBlocklist(ctx context.Context) (int, error) {
	reqCtx, cancel := context.WithTimeout(ctx, disposableEmailSyncTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, disposableEmailSyncSource, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "new-api-email-blocklist-sync")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer CloseResponseBodyGracefully(resp)
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("blocklist upstream returned status %d", resp.StatusCode)
	}

	domains, err := parseDisposableEmailBlocklist(io.LimitReader(resp.Body, disposableEmailMaxFileBytes))
	if err != nil {
		return 0, err
	}
	if len(domains) < disposableEmailBlocklistMinLines {
		return 0, fmt.Errorf("refusing to replace blocklist: upstream returned only %d domains", len(domains))
	}

	path := disposableEmailBlocklistPath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), disposableEmailBlocklistFile+".*.tmp")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	sorted := make([]string, 0, len(domains))
	for domain := range domains {
		sorted = append(sorted, domain)
	}
	slices.Sort(sorted)

	writer := bufio.NewWriter(tmp)
	fmt.Fprintf(writer, "# Synced from %s at %s\n", disposableEmailBlocklistURL, time.Now().UTC().Format(time.RFC3339))
	for _, domain := range sorted {
		writer.WriteString(domain)
		writer.WriteByte('\n')
	}
	if err := writer.Flush(); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Chmod(tmpName, 0644); err != nil {
		return 0, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return 0, err
	}

	replaceDisposableEmailDomains(domains)

	return len(domains), nil
}

// CheckDisposableEmail applies the policy to one address and returns
// ErrDisposableEmailRejected when it must be refused. Addresses that only trip
// a heuristic in log mode are accepted and recorded in the log instead.
func CheckDisposableEmail(ctx context.Context, email string, clientIP string) error {
	domain := emailDomainOf(email)
	if domain == "" {
		return nil
	}
	// Read the option-backed fields under the same lock updateOptionMap takes:
	// a signup concurrent with a settings save must not observe a torn list.
	common.OptionMapRWMutex.RLock()
	allowlist := common.DisposableEmailAllowlist
	extraDomains := common.DisposableEmailExtraDomains
	mode := strings.ToLower(strings.TrimSpace(common.DisposableEmailHeuristicMode))
	enabled := common.DisposableEmailBlockEnabled
	common.OptionMapRWMutex.RUnlock()

	if !enabled {
		return nil
	}

	if matchesEmailDomainList(allowlist, domain) {
		return nil
	}
	if matchesEmailDomainList(extraDomains, domain) {
		logger.LogInfo(ctx, common.LogText("registration email rejected by operator domain list: domain=%s ip=%s", domain, clientIP))
		return ErrDisposableEmailRejected
	}
	if isBlockedEmailDomain(domain) {
		logger.LogInfo(ctx, common.LogText("registration email rejected by disposable domain blocklist: domain=%s ip=%s", domain, clientIP))
		return ErrDisposableEmailRejected
	}

	if mode == DisposableEmailHeuristicOff {
		return nil
	}

	reason := ""
	if parent, hit := recordSourceParentDomain(clientIP, domain); hit {
		reason = "source spread across subdomains of " + parent
	} else if why, hit := randomLabelReason(domain); hit {
		reason = why
	}
	if reason == "" {
		return nil
	}
	if mode == DisposableEmailHeuristicBlock {
		logger.LogWarn(ctx, common.LogText("registration email rejected by heuristic: domain=%s reason=%s ip=%s", domain, reason, clientIP))
		return ErrDisposableEmailRejected
	}
	logger.LogInfo(ctx, common.LogText("registration email would be rejected by heuristic: domain=%s reason=%s ip=%s", domain, reason, clientIP))
	return nil
}

func emailDomainOf(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	at := strings.LastIndexByte(email, '@')
	if at <= 0 || at == len(email)-1 {
		return ""
	}
	return strings.TrimSuffix(email[at+1:], ".")
}

func matchesEmailDomainList(list []string, domain string) bool {
	for _, entry := range list {
		entry = normalizeBlocklistDomain(entry)
		if entry == "" {
			continue
		}
		if domain == entry || strings.HasSuffix(domain, "."+entry) {
			return true
		}
	}
	return false
}

func isBlockedEmailDomain(domain string) bool {
	disposableEmailMu.RLock()
	defer disposableEmailMu.RUnlock()
	if len(disposableEmailDomains) == 0 {
		return false
	}
	labels := strings.Split(domain, ".")
	// Stop at two labels: a bare TLD entry must never match everything below it.
	for i := range len(labels) - 1 {
		if _, ok := disposableEmailDomains[strings.Join(labels[i:], ".")]; ok {
			return true
		}
	}
	return false
}

// randomLabelReason flags domains that look like throwaway infrastructure: a
// generated registrable label under a low-reputation TLD, or a generated label
// anywhere in the chain under such a TLD.
func randomLabelReason(domain string) (string, bool) {
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return "", false
	}
	if _, ok := lowReputationTLDs[labels[len(labels)-1]]; !ok {
		return "", false
	}
	registrable := labels[len(labels)-2]
	if looksGeneratedLabel(registrable) {
		return "generated-label-under-low-reputation-tld", true
	}
	if len(labels) >= 3 && looksGeneratedLabel(labels[len(labels)-3]) {
		return "generated-subdomain-under-low-reputation-tld", true
	}
	return "", false
}

// looksGeneratedLabel reports labels that are almost certainly machine
// generated. Ordinary words and short brand names do not match.
func looksGeneratedLabel(label string) bool {
	if len(label) < 6 || !disposableEmailLabelPattern.MatchString(label) {
		return false
	}
	digits := 0
	for _, r := range label {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	if digits == len(label) {
		return true
	}
	return digits >= 4 && float64(digits)/float64(len(label)) >= 0.34
}

// recordSourceParentDomain tracks which subdomains one registration source
// uses and reports whether it has already spread across the same parent.
//
// Only parents that look like throwaway infrastructure are tracked. A company
// whose staff register as alice@corp.example, bob@corp.example and
// carol@eu.corp.example from one office NAT, or a campus/CGNAT source, must not
// lock the whole parent out for a week. An allowlisted parent never reaches
// here: CheckDisposableEmail returns before tracking when the address matches
// the allowlist.
func recordSourceParentDomain(clientIP, domain string) (string, bool) {
	labels := strings.Split(domain, ".")
	if clientIP == "" || len(labels) < 3 {
		return "", false
	}
	parent := strings.Join(labels[1:], ".")
	if _, lowReputation := lowReputationTLDs[labels[len(labels)-1]]; !lowReputation && !looksGeneratedLabel(labels[1]) {
		return "", false
	}
	blockKey := clientIP + "|" + parent
	now := common.GetTimestamp()

	disposableEmailAutoMu.Lock()
	defer disposableEmailAutoMu.Unlock()

	if expiry, ok := disposableEmailAutoBlocked[blockKey]; ok {
		if expiry > now {
			return parent, true
		}
		delete(disposableEmailAutoBlocked, blockKey)
	}

	pruneSourceTrackersLocked(now)

	record, ok := disposableEmailTrackers[clientIP]
	if !ok {
		record = &sourceDomainRecord{Parents: make(map[string]map[string]int64)}
		disposableEmailTrackers[clientIP] = record
	}
	record.Updated = now
	domains, ok := record.Parents[parent]
	if !ok {
		domains = make(map[string]int64)
		record.Parents[parent] = domains
	}
	domains[domain] = now
	if len(domains) >= disposableEmailParentDomainThreshold {
		disposableEmailAutoBlocked[blockKey] = now + int64(disposableEmailAutoBlockTTL.Seconds())
		return parent, true
	}
	return "", false
}

func pruneSourceTrackersLocked(now int64) {
	if len(disposableEmailTrackers) < disposableEmailTrackerMaxEntries && time.Since(disposableEmailLastPrune) < disposableEmailPruneInterval {
		return
	}
	disposableEmailLastPrune = time.Now()
	window := int64(disposableEmailParentDomainWindow.Seconds())
	for ip, record := range disposableEmailTrackers {
		if now-record.Updated > window {
			delete(disposableEmailTrackers, ip)
			continue
		}
		for parent, domains := range record.Parents {
			for domain, seen := range domains {
				if now-seen > window {
					delete(domains, domain)
				}
			}
			if len(domains) == 0 {
				delete(record.Parents, parent)
			}
		}
		if len(record.Parents) == 0 {
			delete(disposableEmailTrackers, ip)
		}
	}
	for parent, expiry := range disposableEmailAutoBlocked {
		if expiry <= now {
			delete(disposableEmailAutoBlocked, parent)
		}
	}
}

// Package filtering implements a DNS request and response filter.
package filtering

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/AdguardTeam/AdGuardHome/internal/aghnet"
	"github.com/AdguardTeam/dnsproxy/upstream"
	"github.com/AdguardTeam/golibs/cache"
	"github.com/AdguardTeam/golibs/errors"
	"github.com/AdguardTeam/golibs/log"
	"github.com/AdguardTeam/golibs/stringutil"
	"github.com/AdguardTeam/urlfilter"
	"github.com/AdguardTeam/urlfilter/filterlist"
	"github.com/AdguardTeam/urlfilter/rules"
	"github.com/miekg/dns"
)

// The IDs of built-in filter lists.
//
// Keep in sync with client/src/helpers/contants.js.
const (
	CustomListID = -iota
	SysHostsListID
	BlockedSvcsListID
	ParentalListID
	SafeBrowsingListID
	SafeSearchListID
)

// ServiceEntry - blocked service array element
type ServiceEntry struct {
	Name  string
	Rules []*rules.NetworkRule
}

// Settings are custom filtering settings for a client.
type Settings struct {
	ClientName string
	ClientIP   net.IP
	ClientTags []string

	ServicesRules []ServiceEntry

	ProtectionEnabled   bool
	FilteringEnabled    bool
	SafeSearchEnabled   bool
	SafeBrowsingEnabled bool
	ParentalEnabled     bool
}

// Resolver is the interface for net.Resolver to simplify testing.
type Resolver interface {
	LookupIP(ctx context.Context, network, host string) (ips []net.IP, err error)
}

// Config allows you to configure DNS filtering with New() or just change variables directly.
type Config struct {
	// enabled is used to be returned within Settings.
	//
	// It is of type uint32 to be accessed by atomic.
	enabled uint32

	ParentalEnabled     bool `yaml:"parental_enabled"`
	SafeSearchEnabled   bool `yaml:"safesearch_enabled"`
	SafeBrowsingEnabled bool `yaml:"safebrowsing_enabled"`

	SafeBrowsingCacheSize uint `yaml:"safebrowsing_cache_size"` // (in bytes)
	SafeSearchCacheSize   uint `yaml:"safesearch_cache_size"`   // (in bytes)
	ParentalCacheSize     uint `yaml:"parental_cache_size"`     // (in bytes)
	CacheTime             uint `yaml:"cache_time"`              // Element's TTL (in minutes)

	Rewrites []RewriteEntry `yaml:"rewrites"`

	// Names of services to block (globally).
	// Per-client settings can override this configuration.
	BlockedServices []string `yaml:"blocked_services"`

	// EtcHosts is a container of IP-hostname pairs taken from the operating
	// system configuration files (e.g. /etc/hosts).
	EtcHosts *aghnet.HostsContainer `yaml:"-"`

	// Called when the configuration is changed by HTTP request
	ConfigModified func() `yaml:"-"`

	// Register an HTTP handler
	HTTPRegister func(string, string, func(http.ResponseWriter, *http.Request)) `yaml:"-"`

	// CustomResolver is the resolver used by DNSFilter.
	CustomResolver Resolver `yaml:"-"`
}

// LookupStats store stats collected during safebrowsing or parental checks
type LookupStats struct {
	Requests   uint64 // number of HTTP requests that were sent
	CacheHits  uint64 // number of lookups that didn't need HTTP requests
	Pending    int64  // number of currently pending HTTP requests
	PendingMax int64  // maximum number of pending HTTP requests
}

// Stats store LookupStats for safebrowsing, parental and safesearch
type Stats struct {
	Safebrowsing LookupStats
	Parental     LookupStats
	Safesearch   LookupStats
}

// Parameters to pass to filters-initializer goroutine
type filtersInitializerParams struct {
	allowFilters []Filter
	blockFilters []Filter
}

type hostChecker struct {
	check func(host string, qtype uint16, setts *Settings) (res Result, err error)
	name  string
}

// DNSFilter matches hostnames and DNS requests against filtering rules.
type DNSFilter struct {
	rulesStorage         *filterlist.RuleStorage
	filteringEngine      *urlfilter.DNSEngine
	rulesStorageAllow    *filterlist.RuleStorage
	filteringEngineAllow *urlfilter.DNSEngine
	engineLock           sync.RWMutex

	parentalServer       string // access via methods
	safeBrowsingServer   string // access via methods
	parentalUpstream     upstream.Upstream
	safeBrowsingUpstream upstream.Upstream

	safebrowsingCache cache.Cache
	parentalCache     cache.Cache
	safeSearchCache   cache.Cache

	Config // for direct access by library users, even a = assignment
	// confLock protects Config.
	confLock sync.RWMutex

	// Channel for passing data to filters-initializer goroutine
	filtersInitializerChan chan filtersInitializerParams
	filtersInitializerLock sync.Mutex

	// resolver only looks up the IP address of the host while safe search.
	//
	// TODO(e.burkov): Use upstream that configured in dnsforward instead.
	resolver Resolver

	hostCheckers []hostChecker
}

// Filter represents a filter list
type Filter struct {
	ID       int64  // auto-assigned when filter is added (see nextFilterID)
	Data     []byte `yaml:"-"` // List of rules divided by '\n'
	FilePath string `yaml:"-"` // Path to a filtering rules file
}

// Reason holds an enum detailing why it was filtered or not filtered
type Reason int

const (
	// reasons for not filtering

	// NotFilteredNotFound - host was not find in any checks, default value for result
	NotFilteredNotFound Reason = iota
	// NotFilteredAllowList - the host is explicitly allowed
	NotFilteredAllowList
	// NotFilteredError is returned when there was an error during
	// checking.  Reserved, currently unused.
	NotFilteredError

	// reasons for filtering

	// FilteredBlockList - the host was matched to be advertising host
	FilteredBlockList
	// FilteredSafeBrowsing - the host was matched to be malicious/phishing
	FilteredSafeBrowsing
	// FilteredParental - the host was matched to be outside of parental control settings
	FilteredParental
	// FilteredInvalid - the request was invalid and was not processed
	FilteredInvalid
	// FilteredSafeSearch - the host was replaced with safesearch variant
	FilteredSafeSearch
	// FilteredBlockedService - the host is blocked by "blocked services" settings
	FilteredBlockedService

	// Rewritten is returned when there was a rewrite by a legacy DNS rewrite
	// rule.
	Rewritten

	// RewrittenAutoHosts is returned when there was a rewrite by autohosts
	// rules (/etc/hosts and so on).
	RewrittenAutoHosts

	// RewrittenRule is returned when a $dnsrewrite filter rule was applied.
	//
	// TODO(a.garipov): Remove Rewritten and RewrittenAutoHosts by merging their
	// functionality into RewrittenRule.
	//
	// See https://github.com/AdguardTeam/AdGuardHome/issues/2499.
	RewrittenRule
)

// TODO(a.garipov): Resync with actual code names or replace completely
// in HTTP API v1.
var reasonNames = []string{
	NotFilteredNotFound:  "NotFilteredNotFound",
	NotFilteredAllowList: "NotFilteredWhiteList",
	NotFilteredError:     "NotFilteredError",

	FilteredBlockList:      "FilteredBlackList",
	FilteredSafeBrowsing:   "FilteredSafeBrowsing",
	FilteredParental:       "FilteredParental",
	FilteredInvalid:        "FilteredInvalid",
	FilteredSafeSearch:     "FilteredSafeSearch",
	FilteredBlockedService: "FilteredBlockedService",

	Rewritten:          "Rewrite",
	RewrittenAutoHosts: "RewriteEtcHosts",
	RewrittenRule:      "RewriteRule",
}

func (r Reason) String() string {
	if r < 0 || int(r) >= len(reasonNames) {
		return ""
	}

	return reasonNames[r]
}

// In returns true if reasons include r.
func (r Reason) In(reasons ...Reason) (ok bool) {
	for _, reason := range reasons {
		if r == reason {
			return true
		}
	}

	return false
}

// SetEnabled sets the status of the *DNSFilter.
func (d *DNSFilter) SetEnabled(enabled bool) {
	var i int32
	if enabled {
		i = 1
	}
	atomic.StoreUint32(&d.enabled, uint32(i))
}

// GetConfig - get configuration
func (d *DNSFilter) GetConfig() (s Settings) {
	d.confLock.RLock()
	defer d.confLock.RUnlock()

	return Settings{
		FilteringEnabled:    atomic.LoadUint32(&d.Config.enabled) != 0,
		SafeSearchEnabled:   d.Config.SafeSearchEnabled,
		SafeBrowsingEnabled: d.Config.SafeBrowsingEnabled,
		ParentalEnabled:     d.Config.ParentalEnabled,
	}
}

// WriteDiskConfig - write configuration
func (d *DNSFilter) WriteDiskConfig(c *Config) {
	d.confLock.Lock()
	defer d.confLock.Unlock()

	*c = d.Config
	c.Rewrites = cloneRewrites(c.Rewrites)
}

func cloneRewrites(entries []RewriteEntry) (clone []RewriteEntry) {
	return append([]RewriteEntry(nil), entries...)
}

// SetFilters - set new filters (synchronously or asynchronously)
// When filters are set asynchronously, the old filters continue working until the new filters are ready.
//  In this case the caller must ensure that the old filter files are intact.
func (d *DNSFilter) SetFilters(blockFilters, allowFilters []Filter, async bool) error {
	if async {
		params := filtersInitializerParams{
			allowFilters: allowFilters,
			blockFilters: blockFilters,
		}

		d.filtersInitializerLock.Lock() // prevent multiple writers from adding more than 1 task
		// remove all pending tasks
		stop := false
		for !stop {
			select {
			case <-d.filtersInitializerChan:
				//
			default:
				stop = true
			}
		}

		d.filtersInitializerChan <- params
		d.filtersInitializerLock.Unlock()
		return nil
	}

	err := d.initFiltering(allowFilters, blockFilters)
	if err != nil {
		log.Error("Can't initialize filtering subsystem: %s", err)
		return err
	}

	return nil
}

// Starts initializing new filters by signal from channel
func (d *DNSFilter) filtersInitializer() {
	for {
		params := <-d.filtersInitializerChan
		err := d.initFiltering(params.allowFilters, params.blockFilters)
		if err != nil {
			log.Error("Can't initialize filtering subsystem: %s", err)
			continue
		}
	}
}

// Close - close the object
func (d *DNSFilter) Close() {
	d.engineLock.Lock()
	defer d.engineLock.Unlock()
	d.reset()
}

func (d *DNSFilter) reset() {
	var err error

	if d.rulesStorage != nil {
		err = d.rulesStorage.Close()
		if err != nil {
			log.Error("filtering: rulesStorage.Close: %s", err)
		}
	}

	if d.rulesStorageAllow != nil {
		err = d.rulesStorageAllow.Close()
		if err != nil {
			log.Error("filtering: rulesStorageAllow.Close: %s", err)
		}
	}
}

// ResultRule contains information about applied rules.
type ResultRule struct {
	// Text is the text of the rule.
	Text string `json:",omitempty"`
	// IP is the host IP.  It is nil unless the rule uses the
	// /etc/hosts syntax or the reason is FilteredSafeSearch.
	IP net.IP `json:",omitempty"`
	// FilterListID is the ID of the rule's filter list.
	FilterListID int64 `json:",omitempty"`
}

// Result contains the result of a request check.
//
// All fields transitively have omitempty tags so that the query log
// doesn't become too large.
//
// TODO(a.garipov): Clarify relationships between fields.  Perhaps
// replace with a sum type or an interface?
type Result struct {
	// IsFiltered is true if the request is filtered.
	IsFiltered bool `json:",omitempty"`

	// Reason is the reason for blocking or unblocking the request.
	Reason Reason `json:",omitempty"`

	// Rules are applied rules.  If Rules are not empty, each rule is not nil.
	Rules []*ResultRule `json:",omitempty"`

	// IPList is the lookup rewrite result.  It is empty unless Reason is set to
	// Rewritten.
	IPList []net.IP `json:",omitempty"`

	// CanonName is the CNAME value from the lookup rewrite result.  It is empty
	// unless Reason is set to Rewritten or RewrittenRule.
	CanonName string `json:",omitempty"`

	// ServiceName is the name of the blocked service.  It is empty unless
	// Reason is set to FilteredBlockedService.
	ServiceName string `json:",omitempty"`

	// DNSRewriteResult is the $dnsrewrite filter rule result.
	DNSRewriteResult *DNSRewriteResult `json:",omitempty"`
}

// Matched returns true if any match at all was found regardless of
// whether it was filtered or not.
func (r Reason) Matched() bool {
	return r != NotFilteredNotFound
}

// CheckHostRules tries to match the host against filtering rules only.
func (d *DNSFilter) CheckHostRules(host string, qtype uint16, setts *Settings) (Result, error) {
	if !setts.FilteringEnabled {
		return Result{}, nil
	}

	host = strings.ToLower(host)

	return d.matchHost(host, qtype, setts)
}

// CheckHost tries to match the host against filtering rules, then safebrowsing
// and parental control rules, if they are enabled.
func (d *DNSFilter) CheckHost(
	host string,
	qtype uint16,
	setts *Settings,
) (res Result, err error) {
	// Sometimes clients try to resolve ".", which is a request to get root
	// servers.
	if host == "" {
		return Result{}, nil
	}

	host = strings.ToLower(host)

	if setts.FilteringEnabled {
		res = d.processRewrites(host, qtype)
		if res.Reason == Rewritten {
			return res, nil
		}
	}

	for _, hc := range d.hostCheckers {
		res, err = hc.check(host, qtype, setts)
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", hc.name, err)
		}

		if res.Reason.Matched() {
			return res, nil
		}
	}

	return Result{}, nil
}

// matchSysHosts tries to match the host against the operating system's hosts
// database.  err is always nil.
func (d *DNSFilter) matchSysHosts(
	host string,
	qtype uint16,
	setts *Settings,
) (res Result, err error) {
	if !setts.FilteringEnabled || d.EtcHosts == nil {
		return res, nil
	}

	return d.matchSysHostsIntl(&urlfilter.DNSRequest{
		Hostname:         host,
		SortedClientTags: setts.ClientTags,
		// TODO(e.burkov):  Wait for urlfilter update to pass net.IP.
		ClientIP:   setts.ClientIP.String(),
		ClientName: setts.ClientName,
		DNSType:    qtype,
	})
}

// matchSysHostsIntl actually matches the request.  It's separated to avoid
// perfoming checks twice.
func (d *DNSFilter) matchSysHostsIntl(
	req *urlfilter.DNSRequest,
) (res Result, err error) {
	dnsres, _ := d.EtcHosts.MatchRequest(*req)
	if dnsres == nil {
		return res, nil
	}

	dnsr := dnsres.DNSRewrites()
	if len(dnsr) == 0 {
		return res, nil
	}

	res = d.processDNSRewrites(dnsr)
	if cn := res.CanonName; cn != "" {
		// Probably an alias.
		req.Hostname = cn

		return d.matchSysHostsIntl(req)
	}

	res.Reason = RewrittenAutoHosts
	for _, r := range res.Rules {
		r.Text = stringutil.Coalesce(d.EtcHosts.Translate(r.Text), r.Text)
	}

	return res, nil
}

// Process rewrites table
// . Find CNAME for a domain name (exact match or by wildcard)
//  . if found and CNAME equals to domain name - this is an exception;  exit
//  . if found, set domain name to canonical name
//  . repeat for the new domain name (Note: we return only the last CNAME)
// . Find A or AAAA record for a domain name (exact match or by wildcard)
//  . if found, set IP addresses (IPv4 or IPv6 depending on qtype) in Result.IPList array
func (d *DNSFilter) processRewrites(host string, qtype uint16) (res Result) {
	d.confLock.RLock()
	defer d.confLock.RUnlock()

	rr := findRewrites(d.Rewrites, host, qtype)
	if len(rr) != 0 {
		res.Reason = Rewritten
	}

	cnames := stringutil.NewSet()
	origHost := host
	for len(rr) != 0 && rr[0].Type == dns.TypeCNAME {
		log.Debug("rewrite: CNAME for %s is %s", host, rr[0].Answer)

		if host == rr[0].Answer { // "host == CNAME" is an exception
			res.Reason = NotFilteredNotFound

			return res
		}

		host = rr[0].Answer
		if cnames.Has(host) {
			log.Info("rewrite: breaking CNAME redirection loop: %s.  Question: %s", host, origHost)

			return res
		}

		cnames.Add(host)
		res.CanonName = rr[0].Answer
		rr = findRewrites(d.Rewrites, host, qtype)
	}

	for _, r := range rr {
		if r.Type == qtype && (qtype == dns.TypeA || qtype == dns.TypeAAAA) {
			if r.IP == nil { // IP exception
				res.Reason = NotFilteredNotFound

				return res
			}

			res.IPList = append(res.IPList, r.IP)
			log.Debug("rewrite: A/AAAA for %s is %s", host, r.IP)
		}
	}

	return res
}

// matchBlockedServicesRules checks the host against the blocked services rules
// in settings, if any.  The err is always nil, it is only there to make this
// a valid hostChecker function.
func matchBlockedServicesRules(
	host string,
	_ uint16,
	setts *Settings,
) (res Result, err error) {
	if !setts.ProtectionEnabled {
		return Result{}, nil
	}

	svcs := setts.ServicesRules
	if len(svcs) == 0 {
		return Result{}, nil
	}

	req := rules.NewRequestForHostname(host)
	for _, s := range svcs {
		for _, rule := range s.Rules {
			if rule.Match(req) {
				res.Reason = FilteredBlockedService
				res.IsFiltered = true
				res.ServiceName = s.Name

				ruleText := rule.Text()
				res.Rules = []*ResultRule{{
					FilterListID: int64(rule.GetFilterListID()),
					Text:         ruleText,
				}}

				log.Debug("blocked services: matched rule: %s  host: %s  service: %s",
					ruleText, host, s.Name)

				return res, nil
			}
		}
	}

	return res, nil
}

//
// Adding rule and matching against the rules
//

func newRuleStorage(filters []Filter) (rs *filterlist.RuleStorage, err error) {
	lists := make([]filterlist.RuleList, 0, len(filters))
	for _, f := range filters {
		switch id := int(f.ID); {
		case len(f.Data) != 0:
			lists = append(lists, &filterlist.StringRuleList{
				ID:             id,
				RulesText:      string(f.Data),
				IgnoreCosmetic: true,
			})
		case f.FilePath == "":
			continue
		case runtime.GOOS == "windows":
			// On Windows we don't pass a file to urlfilter because it's
			// difficult to update this file while it's being used.
			var data []byte
			data, err = os.ReadFile(f.FilePath)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			} else if err != nil {
				return nil, fmt.Errorf("reading filter content: %w", err)
			}

			lists = append(lists, &filterlist.StringRuleList{
				ID:             id,
				RulesText:      string(data),
				IgnoreCosmetic: true,
			})
		default:
			var list *filterlist.FileRuleList
			list, err = filterlist.NewFileRuleList(id, f.FilePath, true)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			} else if err != nil {
				return nil, fmt.Errorf("creating file rule list with %q: %w", f.FilePath, err)
			}

			lists = append(lists, list)
		}
	}

	rs, err = filterlist.NewRuleStorage(lists)
	if err != nil {
		return nil, fmt.Errorf("creating rule storage: %w", err)
	}

	return rs, nil
}

// Initialize urlfilter objects.
func (d *DNSFilter) initFiltering(allowFilters, blockFilters []Filter) error {
	rulesStorage, err := newRuleStorage(blockFilters)
	if err != nil {
		return err
	}

	rulesStorageAllow, err := newRuleStorage(allowFilters)
	if err != nil {
		return err
	}

	filteringEngine := urlfilter.NewDNSEngine(rulesStorage)
	filteringEngineAllow := urlfilter.NewDNSEngine(rulesStorageAllow)

	func() {
		d.engineLock.Lock()
		defer d.engineLock.Unlock()

		d.reset()
		d.rulesStorage = rulesStorage
		d.filteringEngine = filteringEngine
		d.rulesStorageAllow = rulesStorageAllow
		d.filteringEngineAllow = filteringEngineAllow
	}()

	// Make sure that the OS reclaims memory as soon as possible.
	debug.FreeOSMemory()
	log.Debug("initialized filtering engine")

	return nil
}

// hostRules is a helper that converts a slice of host rules into a slice of the
// rules.Rule interface values.
func hostRulesToRules(netRules []*rules.HostRule) (res []rules.Rule) {
	if netRules == nil {
		return nil
	}

	res = make([]rules.Rule, len(netRules))
	for i, nr := range netRules {
		res[i] = nr
	}

	return res
}

// matchHostProcessAllowList processes the allowlist logic of host
// matching.
func (d *DNSFilter) matchHostProcessAllowList(
	host string,
	dnsres *urlfilter.DNSResult,
) (res Result, err error) {
	var matchedRules []rules.Rule
	if dnsres.NetworkRule != nil {
		matchedRules = []rules.Rule{dnsres.NetworkRule}
	} else if len(dnsres.HostRulesV4) > 0 {
		matchedRules = hostRulesToRules(dnsres.HostRulesV4)
	} else if len(dnsres.HostRulesV6) > 0 {
		matchedRules = hostRulesToRules(dnsres.HostRulesV6)
	}

	if len(matchedRules) == 0 {
		return Result{}, fmt.Errorf("invalid dns result: rules are empty")
	}

	log.Debug("filtering: allowlist rules for host %q: %+v", host, matchedRules)

	return makeResult(matchedRules, NotFilteredAllowList), nil
}

// matchHostProcessDNSResult processes the matched DNS filtering result.
func (d *DNSFilter) matchHostProcessDNSResult(
	qtype uint16,
	dnsres *urlfilter.DNSResult,
) (res Result) {
	if dnsres.NetworkRule != nil {
		reason := FilteredBlockList
		if dnsres.NetworkRule.Whitelist {
			reason = NotFilteredAllowList
		}

		return makeResult([]rules.Rule{dnsres.NetworkRule}, reason)
	}

	if qtype == dns.TypeA && dnsres.HostRulesV4 != nil {
		res = makeResult(hostRulesToRules(dnsres.HostRulesV4), FilteredBlockList)
		for i, hr := range dnsres.HostRulesV4 {
			res.Rules[i].IP = hr.IP.To4()
		}

		return res
	}

	if qtype == dns.TypeAAAA && dnsres.HostRulesV6 != nil {
		res = makeResult(hostRulesToRules(dnsres.HostRulesV6), FilteredBlockList)
		for i, hr := range dnsres.HostRulesV6 {
			res.Rules[i].IP = hr.IP.To16()
		}

		return res
	}

	if dnsres.HostRulesV4 != nil || dnsres.HostRulesV6 != nil {
		// Question type doesn't match the host rules.  Return the first matched
		// host rule, but without an IP address.
		var matchedRules []rules.Rule
		if dnsres.HostRulesV4 != nil {
			matchedRules = []rules.Rule{dnsres.HostRulesV4[0]}
		} else if dnsres.HostRulesV6 != nil {
			matchedRules = []rules.Rule{dnsres.HostRulesV6[0]}
		}

		return makeResult(matchedRules, FilteredBlockList)
	}

	return Result{}
}

// matchHost is a low-level way to check only if hostname is filtered by rules,
// skipping expensive safebrowsing and parental lookups.
func (d *DNSFilter) matchHost(
	host string,
	qtype uint16,
	setts *Settings,
) (res Result, err error) {
	if !setts.FilteringEnabled {
		return Result{}, nil
	}

	ureq := urlfilter.DNSRequest{
		Hostname:         host,
		SortedClientTags: setts.ClientTags,
		// TODO(e.burkov): Wait for urlfilter update to pass net.IP.
		ClientIP:   setts.ClientIP.String(),
		ClientName: setts.ClientName,
		DNSType:    qtype,
	}

	d.engineLock.RLock()
	// Keep in mind that this lock must be held no just when calling Match() but
	// also while using the rules returned by it.
	//
	// TODO(e.burkov):  Inspect if the above is true.
	defer d.engineLock.RUnlock()

	if setts.ProtectionEnabled && d.filteringEngineAllow != nil {
		dnsres, ok := d.filteringEngineAllow.MatchRequest(ureq)
		if ok {
			return d.matchHostProcessAllowList(host, dnsres)
		}
	}

	if d.filteringEngine == nil {
		return Result{}, nil
	}

	dnsres, ok := d.filteringEngine.MatchRequest(ureq)
	// Check DNS rewrites first, because the API there is a bit awkward.
	if dnsr := dnsres.DNSRewrites(); len(dnsr) > 0 {
		res = d.processDNSRewrites(dnsr)
		if res.Reason == RewrittenRule && res.CanonName == host {
			// A rewrite of a host to itself.  Go on and try matching other
			// things.
		} else {
			return res, nil
		}
	} else if !ok {
		return Result{}, nil
	}

	if !setts.ProtectionEnabled {
		// Don't check non-dnsrewrite filtering results.
		return Result{}, nil
	}

	res = d.matchHostProcessDNSResult(qtype, dnsres)
	for _, r := range res.Rules {
		log.Debug(
			"filtering: found rule %q for host %q, filter list id: %d",
			r.Text,
			host,
			r.FilterListID,
		)
	}

	return res, nil
}

// makeResult returns a properly constructed Result.
func makeResult(matchedRules []rules.Rule, reason Reason) (res Result) {
	resRules := make([]*ResultRule, len(matchedRules))
	for i, mr := range matchedRules {
		resRules[i] = &ResultRule{
			FilterListID: int64(mr.GetFilterListID()),
			Text:         mr.Text(),
		}
	}

	return Result{
		IsFiltered: reason == FilteredBlockList,
		Reason:     reason,
		Rules:      resRules,
	}
}

// InitModule manually initializes blocked services map using blockedSvcListID
// as list ID for the rules.
func InitModule() {
	initBlockedServices()
}

// New creates properly initialized DNS Filter that is ready to be used.
func New(c *Config, blockFilters []Filter) (d *DNSFilter) {
	d = &DNSFilter{
		resolver: net.DefaultResolver,
	}
	if c != nil {

		d.safebrowsingCache = cache.New(cache.Config{
			EnableLRU: true,
			MaxSize:   c.SafeBrowsingCacheSize,
		})
		d.safeSearchCache = cache.New(cache.Config{
			EnableLRU: true,
			MaxSize:   c.SafeSearchCacheSize,
		})
		d.parentalCache = cache.New(cache.Config{
			EnableLRU: true,
			MaxSize:   c.ParentalCacheSize,
		})

		if c.CustomResolver != nil {
			d.resolver = c.CustomResolver
		}
	}

	d.hostCheckers = []hostChecker{{
		check: d.matchSysHosts,
		name:  "hosts container",
	}, {
		check: d.matchHost,
		name:  "filtering",
	}, {
		check: matchBlockedServicesRules,
		name:  "blocked services",
	}, {
		check: d.checkSafeBrowsing,
		name:  "safe browsing",
	}, {
		check: d.checkParental,
		name:  "parental",
	}, {
		check: d.checkSafeSearch,
		name:  "safe search",
	}}

	err := d.initSecurityServices()
	if err != nil {
		log.Error("filtering: initialize services: %s", err)
		return nil
	}

	if c != nil {
		d.Config = *c
		d.prepareRewrites()
	}

	bsvcs := []string{}
	for _, s := range d.BlockedServices {
		if !BlockedSvcKnown(s) {
			log.Debug("skipping unknown blocked-service %q", s)
			continue
		}
		bsvcs = append(bsvcs, s)
	}
	d.BlockedServices = bsvcs

	if blockFilters != nil {
		err = d.initFiltering(nil, blockFilters)
		if err != nil {
			log.Error("Can't initialize filtering subsystem: %s", err)
			d.Close()
			return nil
		}
	}

	return d
}

// Start - start the module:
// . start async filtering initializer goroutine
// . register web handlers
func (d *DNSFilter) Start() {
	d.filtersInitializerChan = make(chan filtersInitializerParams, 1)
	go d.filtersInitializer()

	if d.Config.HTTPRegister != nil { // for tests
		d.registerSecurityHandlers()
		d.registerRewritesHandlers()
		d.registerBlockedServicesHandlers()
	}
}

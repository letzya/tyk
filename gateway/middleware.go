package gateway

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strconv"
	"time"

	"github.com/gocraft/health"
	"github.com/justinas/alice"
	newrelic "github.com/newrelic/go-agent"
	"github.com/paulbellamy/ratecounter"
	cache "github.com/pmylund/go-cache"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"

	"github.com/TykTechnologies/tyk/apidef"
	"github.com/TykTechnologies/tyk/config"
	"github.com/TykTechnologies/tyk/request"
	"github.com/TykTechnologies/tyk/storage"
	"github.com/TykTechnologies/tyk/trace"
	"github.com/TykTechnologies/tyk/user"
)

const mwStatusRespond = 666

var (
	GlobalRate            = ratecounter.NewRateCounter(1 * time.Second)
	orgSessionExpiryCache singleflight.Group
)

type TykMiddleware interface {
	Init()
	Base() *BaseMiddleware
	SetName(string)
	SetRequestLogger(*http.Request)
	Logger() *logrus.Entry
	Config() (interface{}, error)
	ProcessRequest(w http.ResponseWriter, r *http.Request, conf interface{}) (error, int) // Handles request
	EnabledForSpec() bool
	Name() string
}

type TraceMiddleware struct {
	TykMiddleware
}

func (tr TraceMiddleware) ProcessRequest(w http.ResponseWriter, r *http.Request, conf interface{}) (error, int) {
	if trace.IsEnabled() {
		span, ctx := trace.Span(r.Context(),
			tr.Name(),
		)
		defer span.Finish()
		return tr.TykMiddleware.ProcessRequest(w, r.WithContext(ctx), conf)
	}
	return tr.TykMiddleware.ProcessRequest(w, r, conf)
}

func createDynamicMiddleware(name string, isPre, useSession bool, baseMid BaseMiddleware) func(http.Handler) http.Handler {
	dMiddleware := &DynamicMiddleware{
		BaseMiddleware:      baseMid,
		MiddlewareClassName: name,
		Pre:                 isPre,
		UseSession:          useSession,
	}

	return createMiddleware(dMiddleware)
}

// Generic middleware caller to make extension easier
func createMiddleware(actualMW TykMiddleware) func(http.Handler) http.Handler {
	mw := &TraceMiddleware{
		TykMiddleware: actualMW,
	}
	// construct a new instance
	mw.Init()
	mw.SetName(mw.Name())
	mw.Logger().Debug("Init")

	// Pull the configuration
	mwConf, err := mw.Config()
	if err != nil {
		mw.Logger().Fatal("[Middleware] Configuration load failed")
	}

	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mw.SetRequestLogger(r)

			if config.Global().NewRelic.AppName != "" {
				if txn, ok := w.(newrelic.Transaction); ok {
					defer newrelic.StartSegment(txn, mw.Name()).End()
				}
			}

			job := instrument.NewJob("MiddlewareCall")
			meta := health.Kvs{}
			eventName := mw.Name() + "." + "executed"

			if instrumentationEnabled {
				meta = health.Kvs{
					"from_ip":  request.RealIP(r),
					"method":   r.Method,
					"endpoint": r.URL.Path,
					"raw_url":  r.URL.String(),
					"size":     strconv.Itoa(int(r.ContentLength)),
					"mw_name":  mw.Name(),
				}
				job.EventKv("executed", meta)
				job.EventKv(eventName, meta)
			}

			startTime := time.Now()
			mw.Logger().WithField("ts", startTime.UnixNano()).Debug("Started")

			if mw.Base().Spec.CORS.OptionsPassthrough && r.Method == "OPTIONS" {
				h.ServeHTTP(w, r)
				return
			}
			err, errCode := mw.ProcessRequest(w, r, mwConf)
			if err != nil {
				// GoPluginMiddleware are expected to send response in case of error
				// but we still want to record error
				_, isGoPlugin := actualMW.(*GoPluginMiddleware)

				handler := ErrorHandler{*mw.Base()}
				handler.HandleError(w, r, err.Error(), errCode, !isGoPlugin)

				meta["error"] = err.Error()

				finishTime := time.Since(startTime)

				if instrumentationEnabled {
					job.TimingKv("exec_time", finishTime.Nanoseconds(), meta)
					job.TimingKv(eventName+".exec_time", finishTime.Nanoseconds(), meta)
				}

				mw.Logger().WithError(err).WithField("code", errCode).WithField("ns", finishTime.Nanoseconds()).Debug("Finished")
				return
			}

			finishTime := time.Since(startTime)

			if instrumentationEnabled {
				job.TimingKv("exec_time", finishTime.Nanoseconds(), meta)
				job.TimingKv(eventName+".exec_time", finishTime.Nanoseconds(), meta)
			}

			mw.Logger().WithField("code", errCode).WithField("ns", finishTime.Nanoseconds()).Debug("Finished")

			// Special code, bypasses all other execution
			if errCode != mwStatusRespond {
				// No error, carry on...
				meta["bypass"] = "1"
				h.ServeHTTP(w, r)
			} else {
				mw.Base().UpdateRequestSession(r)
			}
		})
	}
}

func mwAppendEnabled(chain *[]alice.Constructor, mw TykMiddleware) bool {
	if mw.EnabledForSpec() {
		*chain = append(*chain, createMiddleware(mw))
		return true
	}
	return false
}

func mwList(mws ...TykMiddleware) []alice.Constructor {
	var list []alice.Constructor
	for _, mw := range mws {
		mwAppendEnabled(&list, mw)
	}
	return list
}

// BaseMiddleware wraps up the ApiSpec and Proxy objects to be included in a
// middleware handler, this can probably be handled better.
type BaseMiddleware struct {
	Spec   *APISpec
	Proxy  ReturningHttpHandler
	logger *logrus.Entry
}

func (t BaseMiddleware) Base() *BaseMiddleware { return &t }

func (t BaseMiddleware) Logger() (logger *logrus.Entry) {
	if t.logger == nil {
		t.logger = logrus.NewEntry(log)
	}

	return t.logger
}

func (t *BaseMiddleware) SetName(name string) {
	t.logger = t.Logger().WithField("mw", name)
}

func (t *BaseMiddleware) SetRequestLogger(r *http.Request) {
	t.logger = getLogEntryForRequest(t.Logger(), r, ctxGetAuthToken(r), nil)
}

func (t BaseMiddleware) Init() {}
func (t BaseMiddleware) EnabledForSpec() bool {
	return true
}
func (t BaseMiddleware) Config() (interface{}, error) {
	return nil, nil
}

func (t BaseMiddleware) OrgSession(key string) (user.SessionState, bool) {
	// Try and get the session from the session store
	session, found := t.Spec.OrgSessionManager.SessionDetail(key, false)
	if found && t.Spec.GlobalConfig.EnforceOrgDataAge {
		// If exists, assume it has been authorized and pass on
		// We cache org expiry data
		t.Logger().Debug("Setting data expiry: ", session.OrgID)
		ExpiryCache.Set(session.OrgID, session.DataExpires, cache.DefaultExpiration)
	}

	session.SetKeyHash(storage.HashKey(key))
	return session, found
}

func (t BaseMiddleware) SetOrgExpiry(orgid string, expiry int64) {
	ExpiryCache.Set(orgid, expiry, cache.DefaultExpiration)
}

func (t BaseMiddleware) OrgSessionExpiry(orgid string) int64 {
	t.Logger().Debug("Checking: ", orgid)
	// Cache failed attempt
	id, err, _ := orgSessionExpiryCache.Do(orgid, func() (interface{}, error) {
		cachedVal, found := ExpiryCache.Get(orgid)
		if found {
			return cachedVal, nil
		}
		s, found := t.OrgSession(orgid)
		if found && t.Spec.GlobalConfig.EnforceOrgDataAge {
			return s.DataExpires, nil
		}
		return 0, errors.New("missing session")
	})
	if err != nil {
		t.Logger().Debug("no cached entry found, returning 7 days")
		return int64(604800)
	}
	return id.(int64)
}

func (t BaseMiddleware) UpdateRequestSession(r *http.Request) bool {
	session := ctxGetSession(r)
	token := ctxGetAuthToken(r)

	if session == nil || token == "" {
		return false
	}

	if !ctxSessionUpdateScheduled(r) {
		return false
	}

	lifetime := session.Lifetime(t.Spec.SessionLifetime)
	if err := t.Spec.SessionManager.UpdateSession(token, session, lifetime, false); err != nil {
		t.Logger().WithError(err).Error("Can't update session")
		return false
	}

	// Set context state back
	// Useful for benchmarks when request object stays same
	ctxDisableSessionUpdate(r)

	if !t.Spec.GlobalConfig.LocalSessionCache.DisableCacheSessionState {
		SessionCache.Set(session.KeyHash(), *session, cache.DefaultExpiration)
	}

	return true
}

// ApplyPolicies will check if any policies are loaded. If any are, it
// will overwrite the session state to use the policy values.
func (t BaseMiddleware) ApplyPolicies(session *user.SessionState) error {
	rights := make(map[string]user.AccessDefinition)
	tags := make(map[string]bool)
	didQuota, didRateLimit, didACL := make(map[string]bool), make(map[string]bool), make(map[string]bool)
	policies := session.PolicyIDs()

	for i, polID := range policies {
		policiesMu.RLock()
		policy, ok := policiesByID[polID]
		policiesMu.RUnlock()
		if !ok {
			err := fmt.Errorf("policy not found: %q", polID)
			t.Logger().Error(err)
			return err
		}
		// Check ownership, policy org owner must be the same as API,
		// otherwise youcould overwrite a session key with a policy from a different org!
		if t.Spec != nil && policy.OrgID != t.Spec.OrgID {
			err := fmt.Errorf("attempting to apply policy from different organisation to key, skipping")
			t.Logger().Error(err)
			return err
		}

		if policy.Partitions.PerAPI &&
			(policy.Partitions.Quota || policy.Partitions.RateLimit || policy.Partitions.Acl) {
			err := fmt.Errorf("cannot apply policy %s which has per_api and any of partitions set", policy.ID)
			log.Error(err)
			return err
		}

		if policy.Partitions.PerAPI {
			for apiID, accessRights := range policy.AccessRights {
				// new logic when you can specify quota or rate in more than one policy but for different APIs
				if didQuota[apiID] || didRateLimit[apiID] || didACL[apiID] { // no other partitions allowed
					err := fmt.Errorf("cannot apply multiple policies when some have per_api set and some are partitioned")
					log.Error(err)
					return err
				}

				// check if we already have limit on API level specified when policy was created
				if accessRights.Limit == nil || *accessRights.Limit == (user.APILimit{}) {
					// limit was not specified on API level so we will populate it from policy
					accessRights.Limit = &user.APILimit{
						QuotaMax:           policy.QuotaMax,
						QuotaRenewalRate:   policy.QuotaRenewalRate,
						Rate:               policy.Rate,
						Per:                policy.Per,
						ThrottleInterval:   policy.ThrottleInterval,
						ThrottleRetryLimit: policy.ThrottleRetryLimit,
					}
				}

				// respect current quota renews (on API limit level)
				if r, ok := session.AccessRights[apiID]; ok && r.Limit != nil {
					accessRights.Limit.QuotaRenews = r.Limit.QuotaRenews
				}

				accessRights.AllowanceScope = apiID
				accessRights.Limit.SetBy = apiID

				// overwrite session access right for this API
				rights[apiID] = accessRights

				// identify that limit for that API is set (to allow set it only once)
				didACL[apiID] = true
				didQuota[apiID] = true
				didRateLimit[apiID] = true
			}
		} else {
			usePartitions := policy.Partitions.Quota || policy.Partitions.RateLimit || policy.Partitions.Acl

			for k, v := range policy.AccessRights {
				ar := &v

				if v.Limit == nil {
					v.Limit = &user.APILimit{}
				}

				if !usePartitions || policy.Partitions.Acl {
					didACL[k] = true

					// Merge ACLs for the same API
					if r, ok := rights[k]; ok {
						r.Versions = append(rights[k].Versions, v.Versions...)

						for _, u := range v.AllowedURLs {
							found := false
							for ai, au := range r.AllowedURLs {
								if u.URL == au.URL {
									found = true
									rights[k].AllowedURLs[ai].Methods = append(r.AllowedURLs[ai].Methods, u.Methods...)
								}
							}

							if !found {
								r.AllowedURLs = append(r.AllowedURLs, v.AllowedURLs...)
							}
						}
						r.AllowedURLs = append(r.AllowedURLs, v.AllowedURLs...)

						ar = &r
					}

					ar.Limit.SetBy = policy.ID
				}

				if !usePartitions || policy.Partitions.Quota {
					didQuota[k] = true

					// -1 is special "unlimited" case
					if ar.Limit.QuotaMax != -1 && policy.QuotaMax > ar.Limit.QuotaMax {
						ar.Limit.QuotaMax = policy.QuotaMax
					}

					if policy.QuotaRenewalRate > ar.Limit.QuotaRenewalRate {
						ar.Limit.QuotaRenewalRate = policy.QuotaRenewalRate
					}
				}

				if !usePartitions || policy.Partitions.RateLimit {
					didRateLimit[k] = true

					if ar.Limit.Rate != -1 && policy.Rate > ar.Limit.Rate {
						ar.Limit.Rate = policy.Rate
					}

					if policy.Per > ar.Limit.Per {
						ar.Limit.Per = policy.Per
					}

					if policy.ThrottleInterval > ar.Limit.ThrottleInterval {
						ar.Limit.ThrottleInterval = policy.ThrottleInterval
					}

					if policy.ThrottleRetryLimit > ar.Limit.ThrottleRetryLimit {
						ar.Limit.ThrottleRetryLimit = policy.ThrottleRetryLimit
					}
				}

				// Respect existing QuotaRenews
				if r, ok := session.AccessRights[k]; ok && r.Limit != nil {
					ar.Limit.QuotaRenews = r.Limit.QuotaRenews
				}

				rights[k] = *ar
			}

			// Master policy case
			if len(policy.AccessRights) == 0 {
				if !usePartitions || policy.Partitions.RateLimit {
					session.Rate = policy.Rate
					session.Per = policy.Per
					session.ThrottleInterval = policy.ThrottleInterval
					session.ThrottleRetryLimit = policy.ThrottleRetryLimit
				}

				if !usePartitions || policy.Partitions.Quota {
					session.QuotaMax = policy.QuotaMax
					session.QuotaRenewalRate = policy.QuotaRenewalRate
				}
			}

			if !session.HMACEnabled {
				session.HMACEnabled = policy.HMACEnabled
			}
		}

		// Required for all
		if i == 0 { // if any is true, key is inactive
			session.IsInactive = policy.IsInactive
		} else if policy.IsInactive {
			session.IsInactive = true
		}
		for _, tag := range policy.Tags {
			tags[tag] = true
		}
	}

	for _, tag := range session.Tags {
		tags[tag] = true
	}

	// set tags
	session.Tags = []string{}
	for tag := range tags {
		session.Tags = append(session.Tags, tag)
	}

	distinctACL := map[string]bool{}
	for _, v := range rights {
		if v.Limit.SetBy != "" {
			distinctACL[v.Limit.SetBy] = true
		}
	}

	// If some APIs had only ACL partitions, inherit rest from session level
	for k, v := range rights {
		if !didRateLimit[k] {
			v.Limit.Rate = session.Rate
			v.Limit.Per = session.Per
			v.Limit.ThrottleInterval = session.ThrottleInterval
			v.Limit.ThrottleRetryLimit = session.ThrottleRetryLimit
		}

		if !didQuota[k] {
			v.Limit.QuotaMax = session.QuotaMax
			v.Limit.QuotaRenewalRate = session.QuotaRenewalRate
			v.Limit.QuotaRenews = session.QuotaRenews
		}

		// If multime ACL
		if len(distinctACL) > 1 {
			if v.AllowanceScope == "" && v.Limit.SetBy != "" {
				v.AllowanceScope = v.Limit.SetBy
			}
		}

		v.Limit.SetBy = ""

		rights[k] = v
	}

	// If we have policies defining rules for one single API, update session root vars (legacy)
	if len(didQuota) == 1 && len(didRateLimit) == 1 {
		for _, v := range rights {
			if len(didRateLimit) == 1 {
				session.Rate = v.Limit.Rate
				session.Per = v.Limit.Per
			}

			if len(didQuota) == 1 {
				session.QuotaMax = v.Limit.QuotaMax
				session.QuotaRenews = v.Limit.QuotaRenews
				session.QuotaRenewalRate = v.Limit.QuotaRenewalRate
			}
		}
	}

	// Override session ACL if at least one policy define it
	if len(didACL) > 0 {
		session.AccessRights = rights
	}

	return nil
}

// CheckSessionAndIdentityForValidKey will check first the Session store for a valid key, if not found, it will try
// the Auth Handler, if not found it will fail
func (t BaseMiddleware) CheckSessionAndIdentityForValidKey(key string, r *http.Request) (user.SessionState, bool) {
	minLength := t.Spec.GlobalConfig.MinTokenLength
	if minLength == 0 {
		// See https://github.com/TykTechnologies/tyk/issues/1681
		minLength = 3
	}

	if len(key) <= minLength {
		return user.SessionState{IsInactive: true}, false
	}

	// Try and get the session from the session store
	t.Logger().Debug("Querying local cache")
	cacheKey := key
	if t.Spec.GlobalConfig.HashKeys {
		cacheKey = storage.HashStr(key)
	}

	// Check in-memory cache
	if !t.Spec.GlobalConfig.LocalSessionCache.DisableCacheSessionState {
		cachedVal, found := SessionCache.Get(cacheKey)
		if found {
			t.Logger().Debug("--> Key found in local cache")
			session := cachedVal.(user.SessionState)
			if err := t.ApplyPolicies(&session); err != nil {
				t.Logger().Error(err)
				return session, false
			}
			return session, true
		}
	}

	// Check session store
	t.Logger().Debug("Querying keystore")
	session, found := t.Spec.SessionManager.SessionDetail(key, false)
	if found {
		session.SetKeyHash(cacheKey)
		// If exists, assume it has been authorized and pass on
		// cache it
		if !t.Spec.GlobalConfig.LocalSessionCache.DisableCacheSessionState {
			go SessionCache.Set(cacheKey, session, cache.DefaultExpiration)
		}

		// Check for a policy, if there is a policy, pull it and overwrite the session values
		if err := t.ApplyPolicies(&session); err != nil {
			t.Logger().Error(err)
			return session, false
		}
		t.Logger().Debug("Got key")
		return session, true
	}

	t.Logger().Debug("Querying authstore")
	// 2. If not there, get it from the AuthorizationHandler
	session, found = t.Spec.AuthManager.KeyAuthorised(key)
	if found {
		session.SetKeyHash(cacheKey)
		// If not in Session, and got it from AuthHandler, create a session with a new TTL
		t.Logger().Info("Recreating session for key: ", obfuscateKey(key))

		// cache it
		if !t.Spec.GlobalConfig.LocalSessionCache.DisableCacheSessionState {
			go SessionCache.Set(cacheKey, session, cache.DefaultExpiration)
		}

		// Check for a policy, if there is a policy, pull it and overwrite the session values
		if err := t.ApplyPolicies(&session); err != nil {
			t.Logger().Error(err)
			return session, false
		}

		t.Logger().Debug("Lifetime is: ", session.Lifetime(t.Spec.SessionLifetime))
		ctxScheduleSessionUpdate(r)
	}

	return session, found
}

// FireEvent is added to the BaseMiddleware object so it is available across the entire stack
func (t BaseMiddleware) FireEvent(name apidef.TykEvent, meta interface{}) {
	fireEvent(name, meta, t.Spec.EventPaths)
}

type TykResponseHandler interface {
	Init(interface{}, *APISpec) error
	Name() string
	HandleResponse(http.ResponseWriter, *http.Response, *http.Request, *user.SessionState) error
	HandleError(http.ResponseWriter, *http.Request)
}

func responseProcessorByName(name string) TykResponseHandler {
	switch name {
	case "header_injector":
		return &HeaderInjector{}
	case "response_body_transform":
		return &ResponseTransformMiddleware{}
	case "response_body_transform_jq":
		return &ResponseTransformJQMiddleware{}
	case "header_transform":
		return &HeaderTransform{}
	case "custom_mw_res_hook":
		return &CustomMiddlewareResponseHook{}
	}
	return nil
}

func handleResponseChain(chain []TykResponseHandler, rw http.ResponseWriter, res *http.Response, req *http.Request, ses *user.SessionState) (abortRequest bool, err error) {
	traceIsEnabled := trace.IsEnabled()
	for _, rh := range chain {
		if err := handleResponse(rh, rw, res, req, ses, traceIsEnabled); err != nil {
			// Abort the request if this handler is a response middleware hook:
			if rh.Name() == "CustomMiddlewareResponseHook" {
				rh.HandleError(rw, req)
				return true, err
			}
			return false, err
		}
	}
	return false, nil
}

func handleResponse(rh TykResponseHandler, rw http.ResponseWriter, res *http.Response, req *http.Request, ses *user.SessionState, shouldTrace bool) error {
	if shouldTrace {
		span, ctx := trace.Span(req.Context(), rh.Name())
		defer span.Finish()
		req = req.WithContext(ctx)
	}
	return rh.HandleResponse(rw, res, req, ses)
}

func parseForm(r *http.Request) {
	// https://golang.org/pkg/net/http/#Request.ParseForm
	// ParseForm drains the request body for a request with Content-Type of
	// application/x-www-form-urlencoded
	if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" && r.Form == nil {
		var b bytes.Buffer
		r.Body = ioutil.NopCloser(io.TeeReader(r.Body, &b))

		r.ParseForm()

		r.Body = ioutil.NopCloser(&b)
		return
	}

	r.ParseForm()
}

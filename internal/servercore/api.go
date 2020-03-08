// Package servercore is the core of the IRMA server library, allowing IRMA verifiers, issuers
// or attribute-based signature applications to perform IRMA sessions with irmaclient instances
// (i.e. the IRMA app). It exposes a small interface to expose to other programming languages
// through cgo. It is used by the irmaserver package but otherwise not meant for use in Go.
package servercore

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/alexandrevicenzi/go-sse"
	"github.com/go-errors/errors"
	"github.com/jasonlvhit/gocron"
	"github.com/privacybydesign/irmago"
	"github.com/privacybydesign/irmago/server"
	"github.com/sirupsen/logrus"
)

type Server struct {
	conf             *server.Configuration
	sessions         sessionStore
	scheduler        *gocron.Scheduler
	stopScheduler    chan bool
	handlers         map[string]server.SessionHandler
	serverSentEvents *sse.Server
}

func New(conf *server.Configuration, eventServer *sse.Server) (*Server, error) {
	if err := conf.Check(); err != nil {
		return nil, err
	}

	s := &Server{
		conf:      conf,
		scheduler: gocron.NewScheduler(),
		sessions: &memorySessionStore{
			requestor: make(map[string]*session),
			client:    make(map[string]*session),
			conf:      conf,
		},
		handlers:         make(map[string]server.SessionHandler),
		serverSentEvents: eventServer,
	}

	s.scheduler.Every(10).Seconds().Do(func() {
		s.sessions.deleteExpired()
	})

	s.scheduler.Every(irma.RevocationParameters.RequestorUpdateInterval).Seconds().Do(func() {
		for credid, settings := range s.conf.RevocationSettings {
			if settings.Authority {
				continue
			}
			if err := s.conf.IrmaConfiguration.Revocation.SyncIfOld(credid, settings.Tolerance/2); err != nil {
				s.conf.Logger.Errorf("failed to update revocation database for %s", credid.String())
				_ = server.LogError(err)
			}
		}
	})

	s.stopScheduler = s.scheduler.Start()

	return s, nil
}

func (s *Server) Stop() {
	if err := s.conf.IrmaConfiguration.Revocation.Close(); err != nil {
		_ = server.LogWarning(err)
	}
	s.stopScheduler <- true
	s.sessions.stop()
}

func (s *Server) validateRequest(request irma.SessionRequest) error {
	if _, err := s.conf.IrmaConfiguration.Download(request); err != nil {
		return err
	}
	if err := request.Base().Validate(s.conf.IrmaConfiguration); err != nil {
		return err
	}
	return request.Disclosure().Disclose.Validate(s.conf.IrmaConfiguration)
}

func (s *Server) StartSession(req interface{}, handler server.SessionHandler) (*irma.Qr, string, error) {
	rrequest, err := server.ParseSessionRequest(req)
	if err != nil {
		return nil, "", err
	}

	request := rrequest.SessionRequest()
	action := request.Action()

	if err := s.validateRequest(request); err != nil {
		return nil, "", err
	}

	if action == irma.ActionIssuing {
		if err := s.validateIssuanceRequest(request.(*irma.IssuanceRequest)); err != nil {
			return nil, "", err
		}
	}

	session := s.newSession(action, rrequest)
	s.conf.Logger.WithFields(logrus.Fields{"action": action, "session": session.token}).Infof("Session started")
	if s.conf.Logger.IsLevelEnabled(logrus.DebugLevel) {
		s.conf.Logger.WithFields(logrus.Fields{"session": session.token, "clienttoken": session.clientToken}).Info("Session request: ", server.ToJson(rrequest))
	} else {
		s.conf.Logger.WithFields(logrus.Fields{"session": session.token}).Info("Session request (purged of attribute values): ", server.ToJson(purgeRequest(rrequest)))
	}
	if handler != nil {
		s.handlers[session.token] = handler
	}
	return &irma.Qr{
		Type: action,
		URL:  s.conf.URL + "session/" + session.clientToken,
	}, session.token, nil
}

func (s *Server) GetSessionResult(token string) *server.SessionResult {
	session := s.sessions.get(token)
	if session == nil {
		s.conf.Logger.Warn("Session result requested of unknown session ", token)
		return nil
	}
	return session.result
}

func (s *Server) GetRequest(token string) irma.RequestorRequest {
	session := s.sessions.get(token)
	if session == nil {
		s.conf.Logger.Warn("Session request requested of unknown session ", token)
		return nil
	}
	return session.rrequest
}

func (s *Server) CancelSession(token string) error {
	session := s.sessions.get(token)
	if session == nil {
		return server.LogError(errors.Errorf("can't cancel unknown session %s", token))
	}
	session.handleDelete()
	return nil
}

func (s *Server) Revoke(credid irma.CredentialTypeIdentifier, key string, issued time.Time) error {
	return s.conf.IrmaConfiguration.Revocation.Revoke(credid, key, issued)
}

func Route(path, method string) (component, token, noun string, arg []string, err error) {
	rev := regexp.MustCompile(server.ComponentRevocation + "/(events|updateevents|update|issuancerecord)/?(.*)$")
	matches := rev.FindStringSubmatch(path)
	if len(matches) == 3 {
		args := strings.Split(matches[2], "/")
		return server.ComponentRevocation, "", matches[1], args, nil
	}

	static := regexp.MustCompile(server.ComponentSession + "/(\\w+)$")
	matches = static.FindStringSubmatch(path)
	if len(matches) == 2 && method == http.MethodPost {
		return server.ComponentStatic, matches[1], "", nil, nil
	}

	client := regexp.MustCompile(server.ComponentSession + "/(\\w+)/?(|commitments|proofs|status|statusevents)$")
	matches = client.FindStringSubmatch(path)
	if len(matches) == 3 {
		return server.ComponentSession, matches[1], matches[2], nil, nil
	}

	return "", "", "", nil, server.LogWarning(errors.Errorf("Invalid URL: %s", path))
}

func (s *Server) SubscribeServerSentEvents(w http.ResponseWriter, r *http.Request, token string, requestor bool) error {
	if !s.conf.EnableSSE {
		return errors.New("Server sent events disabled")
	}

	var session *session
	if requestor {
		session = s.sessions.get(token)
	} else {
		session = s.sessions.clientGet(token)
	}
	if session == nil {
		return server.LogError(errors.Errorf("can't subscribe to server sent events of unknown session %s", token))
	}
	if session.status.Finished() {
		return server.LogError(errors.Errorf("can't subscribe to server sent events of finished session %s", token))
	}

	// The EventSource.onopen Javascript callback is not consistently called across browsers (Chrome yes, Firefox+Safari no).
	// However, when the SSE connection has been opened the webclient needs some signal so that it can early detect SSE failures.
	// So we manually send an "open" event. Unfortunately:
	// - we need to give the webclient that connected just now some time, otherwise it will miss the "open" event
	// - the "open" event also goes to all other webclients currently listening, as we have no way to send this
	//   event to just the webclient currently listening. (Thus the handler of this "open" event must be idempotent.)
	go func() {
		time.Sleep(200 * time.Millisecond)
		s.serverSentEvents.SendMessage("session/"+token, sse.NewMessage("", "", "open"))
	}()
	s.serverSentEvents.ServeHTTP(w, r)
	return nil
}

func (s *Server) HandleProtocolMessage(
	path string,
	method string,
	headers map[string][]string,
	message []byte,
) (int, []byte, map[string][]string, *server.SessionResult) {
	var start time.Time
	if s.conf.Verbose >= 2 {
		start = time.Now()
		server.LogRequest("client", method, path, "", headers, message)
	}

	status, output, headers, result := s.handleProtocolMessage(path, method, headers, message)

	if s.conf.Verbose >= 2 {
		l := output
		if http.Header(headers).Get("Content-Type") == "application/octet-stream" {
			l = []byte(hex.EncodeToString(output))
		}
		server.LogResponse(status, time.Now().Sub(start), l)
	}

	return status, output, headers, result
}

func (s *Server) handleProtocolMessage(
	path string,
	method string,
	headers map[string][]string,
	message []byte,
) (status int, output []byte, retheaders map[string][]string, result *server.SessionResult) {
	// Parse path into session and action
	if len(path) > 0 { // Remove any starting and trailing slash
		if path[0] == '/' {
			path = path[1:]
		}
		if path[len(path)-1] == '/' {
			path = path[:len(path)-1]
		}
	}

	component, token, noun, args, err := Route(path, method)
	if err != nil {
		status, output = server.JsonResponse(nil, server.RemoteError(server.ErrorUnsupported, ""))
	}

	switch component {
	case server.ComponentSession:
		status, output, result = s.handleClientMessage(token, noun, method, headers, message)
	case server.ComponentRevocation:
		status, output, retheaders = s.handleRevocationMessage(noun, method, args, headers, message)
	case server.ComponentStatic:
		status, output = s.handleStaticMessage(token, method, message)
	default:
		status, output = server.JsonResponse(nil, server.RemoteError(server.ErrorUnsupported, component))
	}
	return
}

func (s *Server) handleClientMessage(
	token, noun, method string, headers map[string][]string, message []byte,
) (status int, output []byte, result *server.SessionResult) {
	// Fetch the session
	session := s.sessions.clientGet(token)
	if session == nil {
		s.conf.Logger.WithField("clientToken", token).Warn("Session not found")
		status, output = server.JsonResponse(nil, server.RemoteError(server.ErrorSessionUnknown, ""))
		return
	}
	session.Lock()
	defer session.Unlock()

	// However we return, if the session status has been updated
	// then we should inform the user by returning a SessionResult
	defer func() {
		if session.status != session.prevStatus {
			session.prevStatus = session.status
			result = session.result
			if result != nil && result.Status.Finished() {
				if handler := s.handlers[result.Token]; handler != nil {
					go handler(result)
					delete(s.handlers, token)
				}
			}
		}
	}()

	// Route to handler
	var err error
	switch len(noun) {
	case 0:
		if method == http.MethodDelete {
			session.handleDelete()
			status = http.StatusOK
			return
		}
		if method == http.MethodGet {
			status, output = session.checkCache(message, server.StatusConnected)
			if len(output) != 0 {
				return
			}
			h := http.Header(headers)
			min := &irma.ProtocolVersion{}
			max := &irma.ProtocolVersion{}
			if err = json.Unmarshal([]byte(h.Get(irma.MinVersionHeader)), min); err != nil {
				status, output = server.JsonResponse(nil, session.fail(server.ErrorMalformedInput, err.Error()))
				return
			}
			if err = json.Unmarshal([]byte(h.Get(irma.MaxVersionHeader)), max); err != nil {
				status, output = server.JsonResponse(nil, session.fail(server.ErrorMalformedInput, err.Error()))
				return
			}
			status, output = server.JsonResponse(session.handleGetRequest(min, max))
			session.responseCache = responseCache{message: message, response: output, status: status, sessionStatus: server.StatusConnected}
			return
		}
		status, output = server.JsonResponse(nil, session.fail(server.ErrorInvalidRequest, ""))
		return

	default:
		if noun == "statusevents" {
			rerr := server.RemoteError(server.ErrorInvalidRequest, "server sent events not supported by this server")
			status, output = server.JsonResponse(nil, rerr)
			return
		}

		if method == http.MethodGet && noun == "status" {
			status, output = server.JsonResponse(session.handleGetStatus())
			return
		}

		// Below are only POST enpoints
		if method != http.MethodPost {
			status, output = server.JsonResponse(nil, session.fail(server.ErrorInvalidRequest, ""))
			return
		}

		if noun == "commitments" && session.action == irma.ActionIssuing {
			status, output = session.checkCache(message, server.StatusDone)
			if len(output) != 0 {
				return
			}
			commitments := &irma.IssueCommitmentMessage{}
			if err = irma.UnmarshalValidate(message, commitments); err != nil {
				status, output = server.JsonResponse(nil, session.fail(server.ErrorMalformedInput, err.Error()))
				return
			}
			status, output = server.JsonResponse(session.handlePostCommitments(commitments))
			session.responseCache = responseCache{message: message, response: output, status: status, sessionStatus: server.StatusDone}
			return
		}

		if noun == "proofs" && session.action == irma.ActionDisclosing {
			status, output = session.checkCache(message, server.StatusDone)
			if len(output) != 0 {
				return
			}
			disclosure := &irma.Disclosure{}
			if err = irma.UnmarshalValidate(message, disclosure); err != nil {
				status, output = server.JsonResponse(nil, session.fail(server.ErrorMalformedInput, err.Error()))
				return
			}
			status, output = server.JsonResponse(session.handlePostDisclosure(disclosure))
			session.responseCache = responseCache{message: message, response: output, status: status, sessionStatus: server.StatusDone}
			return
		}

		if noun == "proofs" && session.action == irma.ActionSigning {
			status, output = session.checkCache(message, server.StatusDone)
			if len(output) != 0 {
				return
			}
			signature := &irma.SignedMessage{}
			if err = irma.UnmarshalValidate(message, signature); err != nil {
				status, output = server.JsonResponse(nil, session.fail(server.ErrorMalformedInput, err.Error()))
				return
			}
			status, output = server.JsonResponse(session.handlePostSignature(signature))
			session.responseCache = responseCache{message: message, response: output, status: status, sessionStatus: server.StatusDone}
			return
		}

		status, output = server.JsonResponse(nil, session.fail(server.ErrorInvalidRequest, ""))
		return
	}
}

func (s *Server) handleRevocationMessage(
	noun, method string, args []string, headers map[string][]string, message []byte,
) (int, []byte, map[string][]string) {
	if (noun == "events") && method == http.MethodGet {
		if len(args) != 4 {
			return server.BinaryResponse(nil, server.RemoteError(server.ErrorInvalidRequest, "GET events expects 4 url arguments"), nil)
		}
		cred := irma.NewCredentialTypeIdentifier(args[0])
		pkcounter, err := strconv.ParseUint(args[1], 10, 32)
		if err != nil {
			return server.BinaryResponse(nil, server.RemoteError(server.ErrorMalformedInput, err.Error()), nil)
		}
		i, err := strconv.ParseUint(args[2], 10, 64)
		if err != nil {
			return server.BinaryResponse(nil, server.RemoteError(server.ErrorMalformedInput, err.Error()), nil)
		}
		j, err := strconv.ParseUint(args[3], 10, 64)
		if err != nil {
			return server.BinaryResponse(nil, server.RemoteError(server.ErrorMalformedInput, err.Error()), nil)
		}
		return server.BinaryResponse(s.handleGetEvents(cred, uint(pkcounter), i, j))
	}
	if noun == "update" && method == http.MethodGet {
		if len(args) != 2 && len(args) != 3 {
			return server.BinaryResponse(nil, server.RemoteError(server.ErrorInvalidRequest, "GET update expects 2 or 3 url arguments"), nil)
		}
		i, err := strconv.ParseUint(args[1], 10, 64)
		if err != nil {
			return server.BinaryResponse(nil, server.RemoteError(server.ErrorMalformedInput, err.Error()), nil)
		}
		cred := irma.NewCredentialTypeIdentifier(args[0])
		var counter *uint
		if len(args) == 3 {
			i, err := strconv.ParseUint(args[2], 10, 32)
			if err != nil {
				return server.BinaryResponse(nil, server.RemoteError(server.ErrorMalformedInput, err.Error()), nil)
			}
			j := uint(i)
			counter = &j
		}
		updates, rerr, headers := s.handleGetUpdateLatest(cred, i, counter)
		if rerr != nil {
			return server.BinaryResponse(nil, rerr, nil)
		}
		if counter == nil {
			return server.BinaryResponse(updates, rerr, headers)
		} else {
			return server.BinaryResponse(updates[*counter], rerr, headers)
		}
	}
	if noun == "issuancerecord" && method == http.MethodPost {
		if len(args) != 2 {
			return server.BinaryResponse(nil, server.RemoteError(server.ErrorInvalidRequest, "POST issuancercord expects 2 url arguments"), nil)
		}
		cred := irma.NewCredentialTypeIdentifier(args[0])
		counter, err := strconv.ParseUint(args[1], 10, 32)
		if err != nil {
			return server.BinaryResponse(nil, server.RemoteError(server.ErrorMalformedInput, err.Error()), nil)
		}
		status, bts := s.handlePostIssuanceRecord(cred, uint(counter), message)
		return server.BinaryResponse(status, bts, nil)
	}

	return server.BinaryResponse(nil, server.RemoteError(server.ErrorInvalidRequest, ""), nil)
}

func (s *Server) handleStaticMessage(
	id, method string, message []byte,
) (int, []byte) {
	if method != http.MethodPost {
		return server.JsonResponse(nil, server.RemoteError(server.ErrorInvalidRequest, ""))
	}
	rrequest := s.conf.StaticSessionRequests[id]
	if rrequest == nil {
		return server.JsonResponse(nil, server.RemoteError(server.ErrorInvalidRequest, "unknown static session"))
	}
	qr, _, err := s.StartSession(rrequest, s.doResultCallback)
	if err != nil {
		return server.JsonResponse(nil, server.RemoteError(server.ErrorMalformedInput, err.Error()))
	}
	return server.JsonResponse(qr, nil)
}

func (s *Server) doResultCallback(result *server.SessionResult) {
	url := s.GetRequest(result.Token).Base().CallbackURL
	if url == "" {
		return
	}
	server.DoResultCallback(url,
		result,
		s.conf.JwtIssuer,
		s.GetRequest(result.Token).Base().ResultJwtValidity,
		s.conf.JwtRSAPrivateKey,
	)
}

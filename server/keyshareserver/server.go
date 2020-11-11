package keyshareserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jasonlvhit/gocron"
	"github.com/privacybydesign/gabi"
	"github.com/privacybydesign/gabi/big"
	irma "github.com/privacybydesign/irmago"
	"github.com/sirupsen/logrus"

	"github.com/privacybydesign/irmago/internal/common"
	"github.com/privacybydesign/irmago/internal/keysharecore"
	"github.com/privacybydesign/irmago/server"
	"github.com/privacybydesign/irmago/server/irmaserver"

	"github.com/go-chi/chi"
)

type SessionData struct {
	LastKeyID    irma.PublicKeyIdentifier // last used key, used in signing the issuance message
	LastCommitID uint64
	expiry       time.Time
}

// Used to provide context in protocol sessions
type requestAuthorization struct {
	user                  KeyshareUser
	hasValidAuthorization bool
}

type Server struct {
	// configuration
	conf *Configuration

	// external components
	core          *keysharecore.Core
	sessionserver *irmaserver.Server
	db            KeyshareDB

	// Scheduler used to clean sessions
	scheduler     *gocron.Scheduler
	stopScheduler chan<- bool

	// Session data, keeping track of current keyshare protocol session state for each user
	sessions    map[string]*SessionData
	sessionLock sync.Mutex
}

func New(conf *Configuration) (*Server, error) {
	var err error
	s := &Server{
		conf:      conf,
		sessions:  map[string]*SessionData{},
		scheduler: gocron.NewScheduler(),
	}

	// Do initial processing of configuration and create keyshare core
	s.core, err = processConfiguration(conf)
	if err != nil {
		return nil, err
	}

	// Load neccessary idemix keys into core, and ensure that future updates
	// to them are processed
	s.LoadIdemixKeys(conf.ServerConfiguration.IrmaConfiguration)
	conf.ServerConfiguration.IrmaConfiguration.UpdateListeners = append(
		conf.ServerConfiguration.IrmaConfiguration.UpdateListeners,
		s.LoadIdemixKeys)

	// Setup IRMA session server
	s.sessionserver, err = irmaserver.New(conf.ServerConfiguration)
	if err != nil {
		return nil, err
	}

	// Setup DB
	s.db = conf.DB

	// Setup session cache clearing
	s.scheduler.Every(10).Seconds().Do(s.clearSessions)
	s.stopScheduler = s.scheduler.Start()

	return s, nil
}

func (s *Server) Stop() {
	s.stopScheduler <- true
	s.sessionserver.Stop()
}

// clean up any expired sessions
func (s *Server) clearSessions() {
	now := time.Now()
	s.sessionLock.Lock()
	defer s.sessionLock.Unlock()
	for k, v := range s.sessions {
		if now.After(v.expiry) {
			delete(s.sessions, k)
		}
	}
}

func (s *Server) Handler() http.Handler {
	router := chi.NewRouter()

	if s.conf.Verbose >= 2 {
		opts := server.LogOptions{Response: true, Headers: true, From: false, EncodeBinary: true}
		router.Use(server.LogMiddleware("keyshare-app", opts))
	}

	// Registration
	router.Post("/client/register", s.handleRegister)

	// Pin logic
	router.Post("/users/verify/pin", s.handleVerifyPin)
	router.Post("/users/change/pin", s.handleChangePin)

	// Keyshare sessions
	router.Group(func(router chi.Router) {
		router.Use(s.userMiddleware)
		router.Use(s.authorizationMiddleware)
		router.Post("/users/isAuthorized", s.handleValidate)
		router.Post("/prove/getCommitments", s.handleCommitments)
		router.Post("/prove/getResponse", s.handleResponse)
	})

	// IRMA server for issuing myirma credential during registration
	router.Mount("/irma/", s.sessionserver.HandlerFunc())
	return router
}

// On configuration changes, inform the keyshare core of any
// new IRMA issuer public keys.
func (s *Server) LoadIdemixKeys(conf *irma.Configuration) {
	for _, issuer := range conf.Issuers {
		keyIDs, err := conf.PublicKeyIndices(issuer.Identifier())
		if err != nil {
			s.conf.Logger.WithFields(logrus.Fields{"issuer": issuer, "error": err}).Warn("Could not find keyIDs for issuer")
			continue
		}
		for _, id := range keyIDs {
			key, err := conf.PublicKey(issuer.Identifier(), id)
			if err != nil {
				s.conf.Logger.WithFields(logrus.Fields{"keyID": id, "error": err}).Warn("Could not fetch public key for issuer")
				continue
			}
			s.core.DangerousAddTrustedPublicKey(irma.PublicKeyIdentifier{Issuer: issuer.Identifier(), Counter: uint(id)}, key)
		}
	}
}

// /prove/getCommitments
func (s *Server) handleCommitments(w http.ResponseWriter, r *http.Request) {
	// Fetch from context
	user := r.Context().Value("user").(KeyshareUser)
	authorization := r.Context().Value("authorization").(string)

	// Read keys
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		s.conf.Logger.WithField("error", err).Info("Malformed request: could not read request body")
		server.WriteError(w, server.ErrorInvalidRequest, err.Error())
		return
	}
	var keys []irma.PublicKeyIdentifier
	err = json.Unmarshal(body, &keys)
	if err != nil {
		s.conf.Logger.WithField("error", err).Info("Malformed request: could not parse request body")
		s.conf.Logger.WithField("body", body).Debug("Malformed request data")
		server.WriteError(w, server.ErrorInvalidRequest, err.Error())
		return
	}
	if len(keys) == 0 {
		s.conf.Logger.Info("Malformed request: no keys over which to commit specified")
		server.WriteError(w, server.ErrorInvalidRequest, "No key specified")
		return
	}

	commitments, err := s.generateCommitments(user, authorization, keys)
	if err == keysharecore.ErrInvalidChallenge || err == keysharecore.ErrInvalidJWT {
		server.WriteError(w, server.ErrorInvalidRequest, err.Error())
		return
	}
	if err != nil {
		// already logged
		server.WriteError(w, server.ErrorInternal, err.Error())
		return
	}

	server.WriteJson(w, commitments)
}

func (s *Server) generateCommitments(user KeyshareUser, authorization string, keys []irma.PublicKeyIdentifier) (proofPCommitmentMap, error) {
	// Generate commitments
	commitments, commitID, err := s.core.GenerateCommitments(user.Data().Coredata, authorization, keys)
	if err != nil {
		s.conf.Logger.WithField("error", err).Warn("Could not generate commitments for request")
		return proofPCommitmentMap{}, err
	}

	// Prepare output message format
	mappedCommitments := map[string]*gabi.ProofPCommitment{}
	for i, keyID := range keys {
		keyIDV, err := keyID.MarshalText()
		if err != nil {
			s.conf.Logger.WithFields(logrus.Fields{"keyid": keyID, "error": err}).Error("Could not convert key identifier to string")
			return proofPCommitmentMap{}, err
		}
		mappedCommitments[string(keyIDV)] = commitments[i]
	}

	// Store needed data for later requests.
	username := user.Data().Username
	s.sessionLock.Lock()
	if _, ok := s.sessions[username]; !ok {
		s.sessions[username] = &SessionData{}
	}
	s.sessions[username].LastCommitID = commitID
	s.sessions[username].LastKeyID = keys[0]
	s.sessions[username].expiry = time.Now().Add(10 * time.Second)
	s.sessionLock.Unlock()

	// And send response
	return proofPCommitmentMap{Commitments: mappedCommitments}, nil
}

// /prove/getResponse
func (s *Server) handleResponse(w http.ResponseWriter, r *http.Request) {
	// Fetch from context
	user := r.Context().Value("user").(KeyshareUser)
	username := user.Data().Username
	authorization := r.Context().Value("authorization").(string)

	// Read challenge
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		s.conf.Logger.WithField("error", err).Info("Malformed request: could not read request body")
		server.WriteError(w, server.ErrorInvalidRequest, err.Error())
		return
	}
	challenge := new(big.Int)
	err = json.Unmarshal(body, challenge)
	if err != nil {
		s.conf.Logger.Info("Malformed request: could not parse challenge")
		server.WriteError(w, server.ErrorInvalidRequest, err.Error())
		return
	}

	// verify access (avoids leaking whether there is a session ongoing to unauthorized callers)
	if !r.Context().Value("hasValidAuthorization").(bool) {
		s.conf.Logger.Warn("Could not generate keyshare response due to invalid authorization")
		server.WriteError(w, server.ErrorInvalidRequest, "Invalid authorization")
		return
	}

	// Get data from session
	s.sessionLock.Lock()
	sessionData, ok := s.sessions[username]
	s.sessionLock.Unlock()
	if !ok {
		s.conf.Logger.Warn("Request for response without previous call to get commitments")
		server.WriteError(w, server.ErrorInvalidRequest, "Missing previous call to getCommitments")
		return
	}

	// And do the actual responding
	proofResponse, err := s.doGenerateResponses(user, authorization, challenge, sessionData.LastCommitID, sessionData.LastKeyID)
	if err == keysharecore.ErrInvalidChallenge || err == keysharecore.ErrInvalidJWT {
		server.WriteError(w, server.ErrorInvalidRequest, err.Error())
		return
	}
	if err != nil {
		// already logged
		server.WriteError(w, server.ErrorInternal, err.Error())
		return
	}

	server.WriteString(w, proofResponse)
}

func (s *Server) doGenerateResponses(user KeyshareUser, authorization string, challenge *big.Int, commitID uint64, keyID irma.PublicKeyIdentifier) (string, error) {
	// Indicate activity on user account
	err := s.db.SetSeen(user)
	if err != nil {
		s.conf.Logger.WithField("error", err).Error("Could not mark user as seen recently")
		// Do not send to user
	}

	// Make log entry
	err = s.db.AddLog(user, IrmaSession, nil)
	if err != nil {
		s.conf.Logger.WithField("error", err).Error("Could not add log entry for user")
		return "", err
	}

	proofResponse, err := s.core.GenerateResponse(user.Data().Coredata, authorization, commitID, challenge, keyID)
	if err != nil {
		s.conf.Logger.WithField("error", err).Error("Could not generate response for request")
		return "", err
	}

	return proofResponse, nil
}

// /users/isAuthorized
func (s *Server) handleValidate(w http.ResponseWriter, r *http.Request) {
	if r.Context().Value("hasValidAuthorization").(bool) {
		server.WriteJson(w, &keyshareAuthorization{Status: "authorized", Candidates: []string{"pin"}})
	} else {
		server.WriteJson(w, &keyshareAuthorization{Status: "expired", Candidates: []string{"pin"}})
	}
}

// /users/verify/pin
func (s *Server) handleVerifyPin(w http.ResponseWriter, r *http.Request) {
	// Extract request
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		s.conf.Logger.WithField("error", err).Info("Malformed request: could not read request body")
		server.WriteError(w, server.ErrorInvalidRequest, err.Error())
		return
	}
	var msg keysharePinMessage
	err = json.Unmarshal(body, &msg)
	if err != nil {
		s.conf.Logger.WithFields(logrus.Fields{"error": err}).Info("Malformed request: could not parse request body")
		server.WriteError(w, server.ErrorInvalidRequest, err.Error())
		return
	}

	// Fetch user
	user, err := s.db.User(msg.Username)
	if err != nil {
		s.conf.Logger.WithFields(logrus.Fields{"username": msg.Username, "error": err}).Warn("Could not find user in db")
		server.WriteError(w, server.ErrorUserNotRegistered, "")
		return
	}

	// and verify pin
	result, err := s.doVerifyPin(user, msg.Username, msg.Pin)
	if err != nil {
		// already logged
		server.WriteError(w, server.ErrorInternal, err.Error())
		return
	}

	server.WriteJson(w, result)
}

func (s *Server) doVerifyPin(user KeyshareUser, username, pin string) (keysharePinStatus, error) {
	// Check whether timing allows this pin to be checked
	ok, tries, wait, err := s.db.ReservePincheck(user)
	if err != nil {
		s.conf.Logger.WithField("error", err).Error("Could not reserve pin check slot")
		return keysharePinStatus{}, nil
	}
	if !ok {
		err = s.db.AddLog(user, PinCheckRefused, nil)
		if err != nil {
			s.conf.Logger.WithField("error", err).Error("Could not add log entry for user")
			return keysharePinStatus{}, err
		}
		return keysharePinStatus{Status: "error", Message: fmt.Sprintf("%v", wait)}, nil
	}
	// At this point, we are allowed to do an actual check (we have successfully reserved a spot for it), so do it.
	jwtt, err := s.core.ValidatePin(user.Data().Coredata, pin, username)
	if err != nil && err != keysharecore.ErrInvalidPin {
		// Errors other than invalid pin are real errors
		s.conf.Logger.WithField("error", err).Error("Could not validate pin")
		return keysharePinStatus{}, err
	}

	if err == keysharecore.ErrInvalidPin {
		// Handle invalid pin
		err = s.db.AddLog(user, PinCheckFailed, tries)
		if err != nil {
			s.conf.Logger.WithField("error", err).Error("Could not add log entry for user")
			return keysharePinStatus{}, err
		}
		if tries == 0 {
			err = s.db.AddLog(user, PinCheckBlocked, wait)
			if err != nil {
				s.conf.Logger.WithField("error", err).Error("Could not add log entry for user")
				return keysharePinStatus{}, err
			}
			return keysharePinStatus{Status: "error", Message: fmt.Sprintf("%v", wait)}, nil
		} else {
			return keysharePinStatus{Status: "failure", Message: fmt.Sprintf("%v", tries)}, nil
		}
	}

	// Handle success
	err = s.db.ClearPincheck(user)
	if err != nil {
		s.conf.Logger.WithField("error", err).Error("Could not reset users pin check logic")
		// Do not send to user
	}
	err = s.db.SetSeen(user)
	if err != nil {
		s.conf.Logger.WithField("error", err).Error("Could not indicate user activity")
		// Do not send to user
	}
	err = s.db.AddLog(user, PinCheckSuccess, nil)
	if err != nil {
		s.conf.Logger.WithField("error", err).Error("Could not add log entry for user")
		return keysharePinStatus{}, err
	}

	return keysharePinStatus{Status: "success", Message: jwtt}, err
}

// /users/change/pin
func (s *Server) handleChangePin(w http.ResponseWriter, r *http.Request) {
	// Extract request
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		s.conf.Logger.WithField("error", err).Info("Malformed request: could not read request body")
		server.WriteError(w, server.ErrorInvalidRequest, "could not read request body")
		return
	}
	var msg keyshareChangePin
	err = json.Unmarshal(body, &msg)
	if err != nil {
		s.conf.Logger.WithField("error", err).Info("Malformed request: could not parse request body")
		server.WriteError(w, server.ErrorInvalidRequest, "Invalid request")
		return
	}

	// Fetch user
	user, err := s.db.User(msg.Username)
	if err != nil {
		s.conf.Logger.WithFields(logrus.Fields{"username": msg.Username, "error": err}).Warn("Could not find user in db")
		server.WriteError(w, server.ErrorUserNotRegistered, "")
		return
	}

	result, err := s.doUpdatePin(user, msg.OldPin, msg.NewPin)
	if err != nil {
		// already logged
		server.WriteError(w, server.ErrorInternal, err.Error())
		return
	}
	server.WriteJson(w, result)
}

func (s *Server) doUpdatePin(user KeyshareUser, oldPin, newPin string) (keysharePinStatus, error) {
	// Check whether pin check is currently allowed
	ok, tries, wait, err := s.db.ReservePincheck(user)
	if err != nil {
		s.conf.Logger.WithField("error", err).Error("Could not reserve pin check slot")
		return keysharePinStatus{}, err
	}
	if !ok {
		return keysharePinStatus{Status: "error", Message: fmt.Sprintf("%v", wait)}, nil
	}

	// Try to do the update
	user.Data().Coredata, err = s.core.ChangePin(user.Data().Coredata, oldPin, newPin)
	if err == keysharecore.ErrInvalidPin {
		if tries == 0 {
			return keysharePinStatus{Status: "error", Message: fmt.Sprintf("%v", wait)}, nil
		} else {
			return keysharePinStatus{Status: "failure", Message: fmt.Sprintf("%v", tries)}, nil
		}
	} else if err != nil {
		s.conf.Logger.WithField("error", err).Error("Could not change pin")
		return keysharePinStatus{}, nil
	}

	// Mark pincheck as success, resetting users wait and count
	err = s.db.ClearPincheck(user)
	if err != nil {
		s.conf.Logger.WithField("error", err).Error("Could not reset users pin check logic")
		// Do not send to user
	}

	// Write user back
	err = s.db.UpdateUser(user)
	if err != nil {
		s.conf.Logger.WithField("error", err).Error("Could not write updated user to database")
		return keysharePinStatus{}, err
	}

	return keysharePinStatus{Status: "success"}, nil
}

// /client/register
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	// Extract request
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		s.conf.Logger.WithField("error", err).Info("Malformed request: could not read request body")
		server.WriteError(w, server.ErrorInvalidRequest, "could not read request body")
		return
	}
	var msg keyshareEnrollment
	err = json.Unmarshal(body, &msg)
	if err != nil {
		s.conf.Logger.WithField("error", err).Info("Malformed request: could not parse request body")
		server.WriteError(w, server.ErrorInvalidRequest, "Invalid request")
		return
	}

	sessionptr, err := s.doRegistration(msg)
	if err == keysharecore.ErrPinTooLong {
		// Too long pin is not an internal error
		server.WriteError(w, server.ErrorInvalidRequest, err.Error())
		return
	}
	if err != nil {
		// Already logged
		server.WriteError(w, server.ErrorInternal, err.Error())
		return
	}
	server.WriteJson(w, sessionptr)
}

func (s *Server) doRegistration(msg keyshareEnrollment) (*irma.Qr, error) {
	// Generate keyshare server account
	username := generateUsername()
	coredata, err := s.core.GenerateKeyshareSecret(msg.Pin)
	if err != nil {
		s.conf.Logger.WithField("error", err).Error("Could not register user")
		return nil, err
	}
	user, err := s.db.NewUser(KeyshareUserData{Username: username, Language: msg.Language, Coredata: coredata})
	if err != nil {
		s.conf.Logger.WithField("error", err).Error("Could not store new user in database")
		return nil, err
	}

	// Send email if user specified email address
	if msg.Email != nil && *msg.Email != "" && s.conf.EmailServer != "" {
		err = s.sendRegistrationEmail(user, msg.Language, *msg.Email)
		if err != nil {
			// already logged in sendRegistrationEmail
			return nil, err
		}
	}

	// Setup and return issuance session for keyshare credential.
	request := irma.NewIssuanceRequest([]*irma.CredentialRequest{
		{
			CredentialTypeID: irma.NewCredentialTypeIdentifier(s.conf.KeyshareCredential),
			Attributes: map[string]string{
				s.conf.KeyshareAttribute: username,
			},
		}})
	sessionptr, _, err := s.sessionserver.StartSession(request, nil)
	if err != nil {
		s.conf.Logger.WithField("error", err).Error("Could not start keyshare credential issuance sessions")
		return nil, err
	}
	return sessionptr, nil
}

func (s *Server) sendRegistrationEmail(user KeyshareUser, language, email string) error {
	// Fetch template and configuration data for users language, falling back if needed
	template, ok := s.conf.RegistrationEmailTemplates[language]
	if !ok {
		template = s.conf.RegistrationEmailTemplates[s.conf.DefaultLanguage]
	}
	verificationBaseURL, ok := s.conf.VerificationURL[language]
	if !ok {
		verificationBaseURL = s.conf.VerificationURL[s.conf.DefaultLanguage]
	}
	subject, ok := s.conf.RegistrationEmailSubject[language]
	if !ok {
		subject = s.conf.RegistrationEmailSubject[s.conf.DefaultLanguage]
	}

	// Generate token
	token := common.NewSessionToken()

	// Add it to the database
	err := s.db.AddEmailVerification(user, email, token)
	if err != nil {
		s.conf.Logger.WithField("error", err).Error("Could not generate email verifiation mail record")
		return err
	}

	// Build message
	var msg bytes.Buffer
	err = template.Execute(&msg, map[string]string{"VerificationURL": verificationBaseURL + token})
	if err != nil {
		s.conf.Logger.WithField("error", err).Error("Could not generate email verifiation mail")
		return err
	}

	// And send it
	err = server.SendHTMLMail(
		s.conf.EmailServer,
		s.conf.EmailAuth,
		s.conf.EmailFrom,
		email,
		subject,
		msg.Bytes())

	if err != nil {
		s.conf.Logger.WithField("error", err).Error("Could not send email verifiation mail")
		return err
	}

	return nil
}

// Generate a base62 "username".
//  this is a direct port of what the old java server uses.
func generateUsername() string {
	bts := make([]byte, 8)
	_, err := rand.Read(bts)
	if err != nil {
		panic(err)
	}
	raw := make([]byte, 12)
	base64.StdEncoding.Encode(raw, bts)
	return strings.ReplaceAll(
		strings.ReplaceAll(
			strings.ReplaceAll(
				string(raw),
				"+",
				""),
			"/",
			""),
		"=",
		"")
}

func (s *Server) userMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract username from request
		username := r.Header.Get("X-IRMA-Keyshare-Username")

		// and fetch its information
		user, err := s.db.User(username)
		if err != nil {
			s.conf.Logger.WithFields(logrus.Fields{"username": username, "error": err}).Warn("Could not find user in db")
			server.WriteError(w, server.ErrorUserNotRegistered, err.Error())
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), "user", user)))
	})
}

func (s *Server) authorizationMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract authorization from request
		authorization := r.Header.Get("Authorization")
		if strings.HasPrefix(authorization, "Bearer ") {
			authorization = authorization[7:]
		}

		// verify access
		ctx := r.Context()
		err := s.core.ValidateJWT(ctx.Value("user").(KeyshareUser).Data().Coredata, authorization)
		hasValidAuthorization := (err == nil)

		// Construct new context with both authorization and its validity
		nextContext := context.WithValue(
			context.WithValue(ctx, "authorization", authorization),
			"hasValidAuthorization", hasValidAuthorization)

		next.ServeHTTP(w, r.WithContext(nextContext))
	})
}

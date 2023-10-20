// Copyright © 2023 Ory Corp
// SPDX-License-Identifier: Apache-2.0

package registration

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/ory/kratos/selfservice/flow/login"
	"github.com/ory/kratos/ui/node"
	"github.com/ory/x/sqlcon"

	"github.com/tidwall/sjson"

	"github.com/julienschmidt/httprouter"
	"go.opentelemetry.io/otel/trace"

	"github.com/ory/kratos/selfservice/sessiontokenexchange"
	"github.com/ory/kratos/x/events"

	"github.com/pkg/errors"

	"github.com/ory/kratos/driver/config"
	"github.com/ory/kratos/hydra"
	"github.com/ory/kratos/identity"
	"github.com/ory/kratos/selfservice/flow"
	"github.com/ory/kratos/session"
	"github.com/ory/kratos/x"
)

type (
	PreHookExecutor interface {
		ExecuteRegistrationPreHook(w http.ResponseWriter, r *http.Request, a *Flow) error
	}
	PreHookExecutorFunc func(w http.ResponseWriter, r *http.Request, a *Flow) error

	PostHookPostPersistExecutor interface {
		ExecutePostRegistrationPostPersistHook(w http.ResponseWriter, r *http.Request, a *Flow, s *session.Session) error
	}
	PostHookPostPersistExecutorFunc func(w http.ResponseWriter, r *http.Request, a *Flow, s *session.Session) error

	PostHookPrePersistExecutor interface {
		ExecutePostRegistrationPrePersistHook(w http.ResponseWriter, r *http.Request, a *Flow, i *identity.Identity) error
	}
	PostHookPrePersistExecutorFunc func(w http.ResponseWriter, r *http.Request, a *Flow, i *identity.Identity) error

	HooksProvider interface {
		PreRegistrationHooks(ctx context.Context) []PreHookExecutor
		PostRegistrationPrePersistHooks(ctx context.Context, credentialsType identity.CredentialsType) []PostHookPrePersistExecutor
		PostRegistrationPostPersistHooks(ctx context.Context, credentialsType identity.CredentialsType) []PostHookPostPersistExecutor
	}
)

func ExecutorNames[T any](e []T) []string {
	names := make([]string, len(e))
	for k, ee := range e {
		names[k] = fmt.Sprintf("%T", ee)
	}
	return names
}

func (f PreHookExecutorFunc) ExecuteRegistrationPreHook(w http.ResponseWriter, r *http.Request, a *Flow) error {
	return f(w, r, a)
}

func (f PostHookPostPersistExecutorFunc) ExecutePostRegistrationPostPersistHook(w http.ResponseWriter, r *http.Request, a *Flow, s *session.Session) error {
	return f(w, r, a, s)
}

func (f PostHookPrePersistExecutorFunc) ExecutePostRegistrationPrePersistHook(w http.ResponseWriter, r *http.Request, a *Flow, i *identity.Identity) error {
	return f(w, r, a, i)
}

type (
	executorDependencies interface {
		config.Provider
		identity.ManagementProvider
		identity.PrivilegedPoolProvider
		identity.ValidationProvider
		login.FlowPersistenceProvider
		login.StrategyProvider
		session.PersistenceProvider
		session.ManagementProvider
		HooksProvider
		FlowPersistenceProvider
		hydra.Provider
		x.CSRFTokenGeneratorProvider
		x.HTTPClientProvider
		x.LoggingProvider
		x.WriterProvider
		sessiontokenexchange.PersistenceProvider
	}
	HookExecutor struct {
		d executorDependencies
	}
	HookExecutorProvider interface {
		RegistrationExecutor() *HookExecutor
	}
)

func NewHookExecutor(d executorDependencies) *HookExecutor {
	return &HookExecutor{d: d}
}

func (e *HookExecutor) PostRegistrationHook(w http.ResponseWriter, r *http.Request, ct identity.CredentialsType, provider string, a *Flow, i *identity.Identity) error {
	e.d.Logger().
		WithRequest(r).
		WithField("identity_id", i.ID).
		WithField("flow_method", ct).
		Debug("Running PostRegistrationPrePersistHooks.")
	for k, executor := range e.d.PostRegistrationPrePersistHooks(r.Context(), ct) {
		if err := executor.ExecutePostRegistrationPrePersistHook(w, r, a, i); err != nil {
			if errors.Is(err, ErrHookAbortFlow) {
				e.d.Logger().
					WithRequest(r).
					WithField("executor", fmt.Sprintf("%T", executor)).
					WithField("executor_position", k).
					WithField("executors", ExecutorNames(e.d.PostRegistrationPrePersistHooks(r.Context(), ct))).
					WithField("identity_id", i.ID).
					WithField("flow_method", ct).
					Debug("A ExecutePostRegistrationPrePersistHook hook aborted early.")
				return nil
			}

			e.d.Logger().
				WithRequest(r).
				WithField("executor", fmt.Sprintf("%T", executor)).
				WithField("executor_position", k).
				WithField("executors", ExecutorNames(e.d.PostRegistrationPrePersistHooks(r.Context(), ct))).
				WithField("identity_id", i.ID).
				WithField("flow_method", ct).
				WithError(err).
				Error("ExecutePostRegistrationPostPersistHook hook failed with an error.")

			traits := i.Traits
			return flow.HandleHookError(w, r, a, traits, ct.ToUiNodeGroup(), err, e.d, e.d)
		}

		e.d.Logger().WithRequest(r).
			WithField("executor", fmt.Sprintf("%T", executor)).
			WithField("executor_position", k).
			WithField("executors", ExecutorNames(e.d.PostRegistrationPrePersistHooks(r.Context(), ct))).
			WithField("identity_id", i.ID).
			WithField("flow_method", ct).
			Debug("ExecutePostRegistrationPrePersistHook completed successfully.")
	}

	// We need to make sure that the identity has a valid schema before passing it down to the identity pool.
	if err := e.d.IdentityValidator().Validate(r.Context(), i); err != nil {
		return err
		// We're now creating the identity because any of the hooks could trigger a "redirect" or a "session" which
		// would imply that the identity has to exist already.
	} else if err := e.d.IdentityManager().Create(r.Context(), i); err != nil {
		if errors.Is(err, sqlcon.ErrUniqueViolation) {
			strategy, err := e.d.AllLoginStrategies().Strategy(ct)
			if err != nil {
				return err
			}

			_, ok := strategy.(login.LinkableStrategy)

			if ok {
				duplicateIdentifier, err := e.getDuplicateIdentifier(r.Context(), i)
				if err != nil {
					return err
				}
				registrationDuplicateCredentials := flow.RegistrationDuplicateCredentials{
					CredentialsType:     ct,
					CredentialsConfig:   i.Credentials[ct].Config,
					DuplicateIdentifier: duplicateIdentifier,
				}
				loginFlowID, err := a.GetOuterLoginFlowID()
				if err != nil {
					return err
				}
				if loginFlowID != nil {
					loginFlow, err := e.d.LoginFlowPersister().GetLoginFlow(r.Context(), *loginFlowID)
					if err != nil {
						return err
					}
					loginFlow.InternalContext, err = sjson.SetBytes(loginFlow.InternalContext, flow.InternalContextDuplicateCredentialsPath,
						registrationDuplicateCredentials)
					if err != nil {
						return err
					}
					loginFlow.UI.SetNode(node.NewInputField(
						"method",
						node.LoginAndLinkCredentials,
						node.DefaultGroup,
						node.InputAttributeTypeSubmit))
					if err := e.d.LoginFlowPersister().UpdateLoginFlow(r.Context(), loginFlow); err != nil {
						return err
					}
				}

				a.InternalContext, err = sjson.SetBytes(a.InternalContext, flow.InternalContextDuplicateCredentialsPath,
					registrationDuplicateCredentials)
				if err != nil {
					return err
				}
				a.UI.SetNode(node.NewInputField(
					"method",
					node.LoginAndLinkCredentials,
					node.DefaultGroup,
					node.InputAttributeTypeSubmit))
				if err := e.d.RegistrationFlowPersister().UpdateRegistrationFlow(r.Context(), a); err != nil {
					return err
				}
			}
		}
		return err
	}

	// Verify the redirect URL before we do any other processing.
	c := e.d.Config()
	returnTo, err := x.SecureRedirectTo(r, c.SelfServiceBrowserDefaultReturnTo(r.Context()),
		x.SecureRedirectReturnTo(a.ReturnTo),
		x.SecureRedirectUseSourceURL(a.RequestURL),
		x.SecureRedirectAllowURLs(c.SelfServiceBrowserAllowedReturnToDomains(r.Context())),
		x.SecureRedirectAllowSelfServiceURLs(c.SelfPublicURL(r.Context())),
		x.SecureRedirectOverrideDefaultReturnTo(c.SelfServiceFlowRegistrationReturnTo(r.Context(), ct.String())),
	)
	if err != nil {
		return err
	}

	e.d.Audit().
		WithRequest(r).
		WithField("identity_id", i.ID).
		Info("A new identity has registered using self-service registration.")

	trace.SpanFromContext(r.Context()).AddEvent(events.NewRegistrationSucceeded(r.Context(), i.ID, string(a.Type), a.Active.String(), provider))

	s := session.NewInactiveSession()

	s.CompletedLoginForWithProvider(ct, identity.AuthenticatorAssuranceLevel1, provider,
		httprouter.ParamsFromContext(r.Context()).ByName("organization"))
	if err := s.Activate(r, i, c, time.Now().UTC()); err != nil {
		return err
	}

	// We persist the session here so that subsequent hooks (like verification) can use it.
	s.AuthenticatedAt = time.Now().UTC()
	if err := e.d.SessionPersister().UpsertSession(r.Context(), s); err != nil {
		return err
	}

	e.d.Logger().
		WithRequest(r).
		WithField("identity_id", i.ID).
		WithField("flow_method", ct).
		Debug("Running PostRegistrationPostPersistHooks.")
	for k, executor := range e.d.PostRegistrationPostPersistHooks(r.Context(), ct) {
		if err := executor.ExecutePostRegistrationPostPersistHook(w, r, a, s); err != nil {
			if errors.Is(err, ErrHookAbortFlow) {
				e.d.Logger().
					WithRequest(r).
					WithField("executor", fmt.Sprintf("%T", executor)).
					WithField("executor_position", k).
					WithField("executors", ExecutorNames(e.d.PostRegistrationPostPersistHooks(r.Context(), ct))).
					WithField("identity_id", i.ID).
					WithField("flow_method", ct).
					Debug("A ExecutePostRegistrationPostPersistHook hook aborted early.")
				return nil
			}

			e.d.Logger().
				WithRequest(r).
				WithField("executor", fmt.Sprintf("%T", executor)).
				WithField("executor_position", k).
				WithField("executors", ExecutorNames(e.d.PostRegistrationPostPersistHooks(r.Context(), ct))).
				WithField("identity_id", i.ID).
				WithField("flow_method", ct).
				WithError(err).
				Error("ExecutePostRegistrationPostPersistHook hook failed with an error.")

			traits := i.Traits
			return flow.HandleHookError(w, r, a, traits, ct.ToUiNodeGroup(), err, e.d, e.d)
		}

		e.d.Logger().WithRequest(r).
			WithField("executor", fmt.Sprintf("%T", executor)).
			WithField("executor_position", k).
			WithField("executors", ExecutorNames(e.d.PostRegistrationPostPersistHooks(r.Context(), ct))).
			WithField("identity_id", i.ID).
			WithField("flow_method", ct).
			Debug("ExecutePostRegistrationPostPersistHook completed successfully.")
	}

	e.d.Logger().
		WithRequest(r).
		WithField("flow_method", ct).
		WithField("identity_id", i.ID).
		Debug("Post registration execution hooks completed successfully.")

	if a.Type == flow.TypeAPI || x.IsJSONRequest(r) {
		if a.IDToken != "" {
			// We don't want to redirect with the code, if the flow was submitted with an ID token.
			// This is the case for Sign in with native Apple SDK or Google SDK.
		} else if handled, err := e.d.SessionManager().MaybeRedirectAPICodeFlow(w, r, a, s.ID, ct.ToUiNodeGroup()); err != nil {
			return errors.WithStack(err)
		} else if handled {
			return nil
		}

		e.d.Writer().Write(w, r, &APIFlowResponse{
			Identity:     i,
			ContinueWith: a.ContinueWith(),
		})
		return nil
	}

	finalReturnTo := returnTo.String()
	if a.OAuth2LoginChallenge != "" {
		if a.ReturnToVerification != "" {
			// Special case: If Kratos is used as a login UI *and* we want to show the verification UI,
			// redirect to the verification URL first and then return to Hydra.
			finalReturnTo = a.ReturnToVerification
		} else {
			callbackURL, err := e.d.Hydra().AcceptLoginRequest(r.Context(),
				hydra.AcceptLoginRequestParams{
					LoginChallenge:        string(a.OAuth2LoginChallenge),
					IdentityID:            i.ID.String(),
					SessionID:             s.ID.String(),
					AuthenticationMethods: s.AMR,
				})
			if err != nil {
				return err
			}
			finalReturnTo = callbackURL
		}
	} else if a.ReturnToVerification != "" {
		finalReturnTo = a.ReturnToVerification
	}

	x.ContentNegotiationRedirection(w, r, s.Declassified(), e.d.Writer(), finalReturnTo)
	return nil
}

func (e *HookExecutor) getDuplicateIdentifier(ctx context.Context, i *identity.Identity) (string, error) {
	for ct, credentials := range i.Credentials {
		for _, identifier := range credentials.Identifiers {
			_, _, err := e.d.PrivilegedIdentityPool().FindByCredentialsIdentifier(ctx, ct, identifier)
			if err != nil {
				if errors.Is(err, sqlcon.ErrNoRows) {
					continue
				}
				return "", err
			}
			return identifier, nil
		}
	}
	return "", errors.New("Duplicate credential not found")
}

func (e *HookExecutor) PreRegistrationHook(w http.ResponseWriter, r *http.Request, a *Flow) error {
	for _, executor := range e.d.PreRegistrationHooks(r.Context()) {
		if err := executor.ExecuteRegistrationPreHook(w, r, a); err != nil {
			return err
		}
	}

	return nil
}

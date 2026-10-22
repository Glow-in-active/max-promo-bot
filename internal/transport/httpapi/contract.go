package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/getkin/kin-openapi/openapi3filter"
	nethttpmiddleware "github.com/oapi-codegen/nethttp-middleware"
	"promo/internal/transport/httpapi/generated"
)

var _ generated.ServerInterface = (*server)(nil)

func (s *server) contractHandler() http.Handler {
	spec, err := generated.GetSwagger()
	if err != nil {
		panic("load embedded OpenAPI contract: " + err.Error())
	}
	if err = spec.Validate(context.Background()); err != nil {
		panic("invalid embedded OpenAPI contract: " + err.Error())
	}
	router := generated.HandlerWithOptions(s, generated.StdHTTPServerOptions{
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, _ error) {
			writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request parameters.")
		},
	})
	return nethttpmiddleware.OapiRequestValidatorWithOptions(spec, &nethttpmiddleware.Options{
		DoNotValidateServers: true,
		Options:              openapi3filter.Options{AuthenticationFunc: s.authenticate},
		ErrorHandlerWithOpts: contractError,
	})(router)
}

func (s *server) authenticate(_ context.Context, input *openapi3filter.AuthenticationInput) error {
	request := input.RequestValidationInput.Request
	recordRoute(request, input.RequestValidationInput.Route.Path)
	var valid bool
	switch input.SecuritySchemeName {
	case "AdminBearer":
		valid = authenticated(bearer(request), s.config.AdminToken)
	case "DemoBearer":
		valid = authenticated(bearer(request), s.config.DemoToken)
	case "WebhookSecret":
		valid = authenticated(request.Header.Get("X-Max-Bot-Api-Secret"), s.config.WebhookSecret)
	}
	if !valid {
		return errors.New("authentication required")
	}
	return nil
}

func contractError(_ context.Context, err error, w http.ResponseWriter, r *http.Request, options nethttpmiddleware.ErrorHandlerOpts) {
	if options.MatchedRoute != nil {
		recordRoute(r, options.MatchedRoute.Route.Path)
	}

	var size *http.MaxBytesError
	if errors.As(err, &size) {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Request body is too large.")
		return
	}

	var security *openapi3filter.SecurityRequirementsError
	if errors.As(err, &security) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "unauthorized", "A valid bearer token or webhook secret is required.")
		return
	}
	if options.StatusCode == http.StatusNotFound || options.StatusCode == http.StatusMethodNotAllowed {
		writeError(w, options.StatusCode, "not_found", "Route not found.")
		return
	}
	if options.MatchedRoute != nil && options.MatchedRoute.Route.Operation.RequestBody != nil &&
		strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json.")
		return
	}
	writeError(w, http.StatusBadRequest, "invalid_request", "Request does not match the API contract.")
}

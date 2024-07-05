// LXD external REST API
//
// This is the REST API used by all LXD clients.
// Internal endpoints aren't included in this documentation.
//
// The LXD API is available over both a local unix+http and remote https API.
// Authentication for local users relies on group membership and access to the unix socket.
// For remote users, the default authentication method is TLS client
// certificates with a macaroon based (candid) authentication method also
// supported.
//
//     Version: 1.0
//     License: Apache-2.0 https://www.apache.org/licenses/LICENSE-2.0
//     Contact: LXD upstream <lxc-devel@lists.linuxcontainers.org> https://github.com/canonical/lxd
//
// swagger:meta
package main

import (
	"github.com/canonical/lxd/shared/api"
)

// Common error definitions.

// Operation
//
// swagger:response Operation
type swaggerOperation struct {
	// Empty sync response
	// in: body
	Body struct {
		// Example: async
		Type string `json:"type"`

		// Example: Operation created
		Status string `json:"status"`

		// Example: 100
		StatusCode int `json:"status_code"`

		// Example: /1.0/operations/66e83638-9dd7-4a26-aef2-5462814869a1
		Operation string `json:"operation"`

		Metadata api.Operation `json:"metadata"`
	}
}

// Empty sync response
//
// swagger:response EmptySyncResponse
type swaggerEmptySyncResponse struct {
	// Empty sync response
	// in: body
	Body struct {
		// Example: sync
		Type string `json:"type"`

		// Example: Success
		Status string `json:"status"`

		// Example: 200
		StatusCode int `json:"status_code"`
	}
}

// Bad Request
//
// swagger:response BadRequest
type swaggerBadRequest struct {
	// Bad Request
	// in: body
	Body struct {
		// Example: error
		Type string `json:"type"`

		// Example: 400
		Code int `json:"code"`

		// Example: bad request
		Error string `json:"error"`
	}
}

// Forbidden
//
// swagger:response Forbidden
type swaggerForbidden struct {
	// Bad Request
	// in: body
	Body struct {
		// Example: error
		Type string `json:"type"`

		// Example: 403
		Code int `json:"code"`

		// Example: not authorized
		Error string `json:"error"`
	}
}

// Precondition Failed
//
// swagger:response PreconditionFailed
type swaggerPreconditionFailed struct {
	// Internal server Error
	// in: body
	Body struct {
		// Example: error
		Type string `json:"type"`

		// Example: 412
		Code int `json:"code"`

		// Example: precondition failed
		Error string `json:"error"`
	}
}

// Internal Server Error
//
// swagger:response InternalServerError
type swaggerInternalServerError struct {
	// Internal server Error
	// in: body
	Body struct {
		// Example: error
		Type string `json:"type"`

		// Example: 500
		Code int `json:"code"`

		// Example: internal server error
		Error string `json:"error"`
	}
}

// Not found
//
// swagger:response NotFound
type swaggerNotFound struct {
	// Not found
	// in: body
	Body struct {
		// Example: error
		Type string `json:"type"`

		// Example: 404
		Code int `json:"code"`

		// Example: not found
		Error string `json:"error"`
	}
}

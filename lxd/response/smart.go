package response

import (
	"database/sql"
	"errors"
	"net/http"
	"os"

	pkgErrors "github.com/pkg/errors"

	"github.com/canonical/lxd/lxd/db"
	"github.com/canonical/lxd/shared/api"
)

var httpResponseErrors = map[int][]error{
	http.StatusNotFound:  {os.ErrNotExist, sql.ErrNoRows, db.ErrNoSuchObject},
	http.StatusForbidden: {os.ErrPermission},
	http.StatusConflict:  {db.ErrAlreadyDefined},
}

// SmartError returns the right error message based on err.
// It uses both the stdlib errors and the github.com/pkg/errors packages to unwrap the error and find the cause.
func SmartError(err error) Response {
	if err == nil {
		return EmptySyncResponse
	}

	if statusCode, found := api.StatusErrorMatch(err); found {
		return &errorResponse{statusCode, err.Error()}
	}

	for httpStatusCode, checkErrs := range httpResponseErrors {
		for _, checkErr := range checkErrs {
			if errors.Is(err, checkErr) || pkgErrors.Cause(err) == checkErr {
				if err != checkErr {
					// If the error has been wrapped return the top-level error message.
					return &errorResponse{httpStatusCode, err.Error()}
				}

				// If the error hasn't been wrapped, replace the error message with the generic
				// HTTP status text.
				return &errorResponse{httpStatusCode, http.StatusText(httpStatusCode)}
			}
		}
	}

	return &errorResponse{http.StatusInternalServerError, err.Error()}
}

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
	log "gopkg.in/inconshreveable/log15.v2"

	"github.com/canonical/lxd/lxd/db"
	"github.com/canonical/lxd/lxd/lifecycle"
	"github.com/canonical/lxd/lxd/operations"
	projecthelpers "github.com/canonical/lxd/lxd/project"
	"github.com/canonical/lxd/lxd/rbac"
	"github.com/canonical/lxd/lxd/request"
	"github.com/canonical/lxd/lxd/response"
	"github.com/canonical/lxd/lxd/state"
	"github.com/canonical/lxd/lxd/util"
	"github.com/canonical/lxd/shared"
	"github.com/canonical/lxd/shared/api"
	"github.com/canonical/lxd/shared/validate"
	"github.com/canonical/lxd/shared/version"
)

var projectFeatures = []string{"features.images", "features.profiles", "features.storage.volumes"}

var projectsCmd = APIEndpoint{
	Path: "projects",

	Get:  APIEndpointAction{Handler: projectsGet, AccessHandler: allowAuthenticated},
	Post: APIEndpointAction{Handler: projectsPost},
}

var projectCmd = APIEndpoint{
	Path: "projects/{name}",

	Delete: APIEndpointAction{Handler: projectDelete},
	Get:    APIEndpointAction{Handler: projectGet, AccessHandler: allowAuthenticated},
	Patch:  APIEndpointAction{Handler: projectPatch, AccessHandler: allowAuthenticated},
	Post:   APIEndpointAction{Handler: projectPost},
	Put:    APIEndpointAction{Handler: projectPut, AccessHandler: allowAuthenticated},
}

// swagger:operation GET /1.0/projects projects projects_get
//
// Get the projects
//
// Returns a list of projects (URLs).
//
// ---
// produces:
//   - application/json
// responses:
//   "200":
//     description: API endpoints
//     schema:
//       type: object
//       description: Sync response
//       properties:
//         type:
//           type: string
//           description: Response type
//           example: sync
//         status:
//           type: string
//           description: Status description
//           example: Success
//         status_code:
//           type: integer
//           description: Status code
//           example: 200
//         metadata:
//           type: array
//           description: List of endpoints
//           items:
//             type: string
//           example: |-
//             [
//               "/1.0/projects/default",
//               "/1.0/projects/foo"
//             ]
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"

// swagger:operation GET /1.0/projects?recursion=1 projects projects_get_recursion1
//
// Get the projects
//
// Returns a list of projects (structs).
//
// ---
// produces:
//   - application/json
// responses:
//   "200":
//     description: API endpoints
//     schema:
//       type: object
//       description: Sync response
//       properties:
//         type:
//           type: string
//           description: Response type
//           example: sync
//         status:
//           type: string
//           description: Status description
//           example: Success
//         status_code:
//           type: integer
//           description: Status code
//           example: 200
//         metadata:
//           type: array
//           description: List of projects
//           items:
//             $ref: "#/definitions/Project"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func projectsGet(d *Daemon, r *http.Request) response.Response {
	recursion := util.IsRecursionRequest(r)

	var result interface{}
	err := d.cluster.Transaction(func(tx *db.ClusterTx) error {
		filter := db.ProjectFilter{}
		if recursion {
			projects, err := tx.GetProjects(filter)
			if err != nil {
				return err
			}

			filtered := []api.Project{}
			for _, project := range projects {
				if !rbac.UserHasPermission(r, project.Name, "view") {
					continue
				}

				filtered = append(filtered, project.ToAPI())
			}

			result = filtered
		} else {
			uris, err := tx.GetProjectURIs(filter)
			if err != nil {
				return err
			}

			filtered := []string{}
			for _, uri := range uris {
				name := strings.Split(uri, "/1.0/projects/")[1]

				if !rbac.UserHasPermission(r, name, "view") {
					continue
				}

				filtered = append(filtered, uri)
			}

			result = filtered
		}

		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	return response.SyncResponse(true, result)
}

// swagger:operation POST /1.0/projects projects projects_post
//
// Add a project
//
// Creates a new project.
//
// ---
// consumes:
//   - application/json
// produces:
//   - application/json
// parameters:
//   - in: body
//     name: project
//     description: Project
//     required: true
//     schema:
//       $ref: "#/definitions/ProjectsPost"
// responses:
//   "200":
//     $ref: "#/responses/EmptySyncResponse"
//   "400":
//     $ref: "#/responses/BadRequest"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func projectsPost(d *Daemon, r *http.Request) response.Response {
	// Parse the request.
	project := db.Project{}

	// Set default features.
	if project.Config == nil {
		project.Config = map[string]string{}
	}
	for _, feature := range projectFeatures {
		_, ok := project.Config[feature]
		if !ok {
			project.Config[feature] = "true"
		}
	}

	err := json.NewDecoder(r.Body).Decode(&project)
	if err != nil {
		return response.BadRequest(err)
	}

	// Quick checks.
	err = projectValidateName(project.Name)
	if err != nil {
		return response.BadRequest(err)
	}

	// Validate the configuration.
	err = projectValidateConfig(d.State(), project.Config)
	if err != nil {
		return response.BadRequest(err)
	}

	var id int64
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		id, err = tx.CreateProject(project)
		if err != nil {
			return fmt.Errorf("Failed adding database record: %w", err)
		}

		if shared.IsTrue(project.Config["features.profiles"]) {
			err = projectCreateDefaultProfile(tx, project.Name)
			if err != nil {
				return err
			}

			if project.Config["features.images"] == "false" {
				err = tx.InitProjectWithoutImages(project.Name)
				if err != nil {
					return err
				}
			}
		}

		return nil
	})
	if err != nil {
		return response.SmartError(fmt.Errorf("Failed creating project %q: %w", project.Name, err))
	}

	if d.rbac != nil {
		err = d.rbac.AddProject(id, project.Name)
		if err != nil {
			return response.SmartError(err)
		}
	}

	requestor := request.CreateRequestor(r)
	d.State().Events.SendLifecycle(project.Name, lifecycle.ProjectCreated.Event(project.Name, requestor, nil))

	return response.SyncResponseLocation(true, nil, fmt.Sprintf("/%s/projects/%s", version.APIVersion, project.Name))
}

// Create the default profile of a project.
func projectCreateDefaultProfile(tx *db.ClusterTx, project string) error {
	// Create a default profile
	profile := db.Profile{}
	profile.Project = project
	profile.Name = projecthelpers.Default
	profile.Description = fmt.Sprintf("Default LXD profile for project %s", project)

	_, err := tx.CreateProfile(profile)
	if err != nil {
		return fmt.Errorf("Add default profile to database: %w", err)
	}
	return nil
}

// swagger:operation GET /1.0/projects/{name} projects project_get
//
// Get the project
//
// Gets a specific project.
//
// ---
// produces:
//   - application/json
// responses:
//   "200":
//     description: Project
//     schema:
//       type: object
//       description: Sync response
//       properties:
//         type:
//           type: string
//           description: Response type
//           example: sync
//         status:
//           type: string
//           description: Status description
//           example: Success
//         status_code:
//           type: integer
//           description: Status code
//           example: 200
//         metadata:
//           $ref: "#/definitions/Project"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func projectGet(d *Daemon, r *http.Request) response.Response {
	name := mux.Vars(r)["name"]

	// Check user permissions
	if !rbac.UserHasPermission(r, name, "view") {
		return response.Forbidden(nil)
	}

	// Get the database entry
	project, err := d.cluster.GetProject(name)
	if err != nil {
		return response.SmartError(err)
	}

	etag := []interface{}{
		project.Description,
		project.Config,
	}

	return response.SyncResponseETag(true, project.ToAPI(), etag)
}

// swagger:operation PUT /1.0/projects/{name} projects project_put
//
// Update the project
//
// Updates the entire project configuration.
//
// ---
// consumes:
//   - application/json
// produces:
//   - application/json
// parameters:
//   - in: body
//     name: project
//     description: Project configuration
//     required: true
//     schema:
//       $ref: "#/definitions/ProjectPut"
// responses:
//   "200":
//     $ref: "#/responses/EmptySyncResponse"
//   "400":
//     $ref: "#/responses/BadRequest"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "412":
//     $ref: "#/responses/PreconditionFailed"
//   "500":
//     $ref: "#/responses/InternalServerError"
func projectPut(d *Daemon, r *http.Request) response.Response {
	name := mux.Vars(r)["name"]

	// Check user permissions
	if !rbac.UserHasPermission(r, name, "manage-projects") {
		return response.Forbidden(nil)
	}

	// Get the current data
	project, err := d.cluster.GetProject(name)
	if err != nil {
		return response.SmartError(err)
	}

	// Validate ETag
	etag := []interface{}{
		project.Description,
		project.Config,
	}
	err = util.EtagCheck(r, etag)
	if err != nil {
		return response.PreconditionFailed(err)
	}

	// Parse the request
	req := api.ProjectPut{}

	err = json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	requestor := request.CreateRequestor(r)
	d.State().Events.SendLifecycle(project.Name, lifecycle.ProjectUpdated.Event(project.Name, requestor, nil))

	return projectChange(d, project, req)
}

// swagger:operation PATCH /1.0/projects/{name} projects project_patch
//
// Partially update the project
//
// Updates a subset of the project configuration.
//
// ---
// consumes:
//   - application/json
// produces:
//   - application/json
// parameters:
//   - in: body
//     name: project
//     description: Project configuration
//     required: true
//     schema:
//       $ref: "#/definitions/ProjectPut"
// responses:
//   "200":
//     $ref: "#/responses/EmptySyncResponse"
//   "400":
//     $ref: "#/responses/BadRequest"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "412":
//     $ref: "#/responses/PreconditionFailed"
//   "500":
//     $ref: "#/responses/InternalServerError"
func projectPatch(d *Daemon, r *http.Request) response.Response {
	name := mux.Vars(r)["name"]

	// Check user permissions
	if !rbac.UserHasPermission(r, name, "manage-projects") {
		return response.Forbidden(nil)
	}

	// Get the current data
	project, err := d.cluster.GetProject(name)
	if err != nil {
		return response.SmartError(err)
	}

	// Validate ETag
	etag := []interface{}{
		project.Description,
		project.Config,
	}
	err = util.EtagCheck(r, etag)
	if err != nil {
		return response.PreconditionFailed(err)
	}

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return response.InternalError(err)
	}

	rdr1 := ioutil.NopCloser(bytes.NewBuffer(body))
	rdr2 := ioutil.NopCloser(bytes.NewBuffer(body))

	reqRaw := shared.Jmap{}
	if err := json.NewDecoder(rdr1).Decode(&reqRaw); err != nil {
		return response.BadRequest(err)
	}

	req := api.ProjectPut{}
	if err := json.NewDecoder(rdr2).Decode(&req); err != nil {
		return response.BadRequest(err)
	}

	// Check what was actually set in the query
	_, err = reqRaw.GetString("description")
	if err != nil {
		req.Description = project.Description
	}

	config, err := reqRaw.GetMap("config")
	if err != nil {
		req.Config = project.Config
	} else {
		for k, v := range project.Config {
			_, ok := config[k]
			if !ok {
				config[k] = v
			}
		}
	}

	requestor := request.CreateRequestor(r)
	d.State().Events.SendLifecycle(project.Name, lifecycle.ProjectUpdated.Event(project.Name, requestor, nil))

	return projectChange(d, project, req)
}

// Common logic between PUT and PATCH.
func projectChange(d *Daemon, project *db.Project, req api.ProjectPut) response.Response {
	// Make a list of config keys that have changed.
	configChanged := []string{}
	for key := range project.Config {
		if req.Config[key] != project.Config[key] {
			configChanged = append(configChanged, key)
		}
	}

	for key := range req.Config {
		_, ok := project.Config[key]
		if !ok {
			configChanged = append(configChanged, key)
		}
	}

	// Flag indicating if any feature has changed.
	featuresChanged := false
	for _, featureKey := range projectFeatures {
		if shared.StringInSlice(featureKey, configChanged) {
			featuresChanged = true
			break
		}
	}

	// Quick checks.
	if project.Name == projecthelpers.Default && featuresChanged {
		return response.BadRequest(fmt.Errorf("You can't change the features of the default project"))
	}

	if !projectIsEmpty(project) && featuresChanged {
		return response.BadRequest(fmt.Errorf("Features can only be changed on empty projects"))
	}

	// Validate the configuration.
	err := projectValidateConfig(d.State(), req.Config)
	if err != nil {
		return response.BadRequest(err)
	}

	// Update the database entry.
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		err := projecthelpers.AllowProjectUpdate(tx, project.Name, req.Config, configChanged)
		if err != nil {
			return err
		}

		err = tx.UpdateProject(project.Name, req)
		if err != nil {
			return fmt.Errorf("Persist profile changes: %w", err)
		}

		if shared.StringInSlice("features.profiles", configChanged) {
			if shared.IsTrue(req.Config["features.profiles"]) {
				err = projectCreateDefaultProfile(tx, project.Name)
				if err != nil {
					return err
				}
			} else {
				// Delete the project-specific default profile.
				err = tx.DeleteProfile(project.Name, projecthelpers.Default)
				if err != nil {
					return fmt.Errorf("Delete project default profile: %w", err)
				}
			}
		}

		return nil
	})

	if err != nil {
		return response.SmartError(err)
	}

	return response.EmptySyncResponse
}

// swagger:operation POST /1.0/projects/{name} projects project_post
//
// Rename the project
//
// Renames an existing project.
//
// ---
// consumes:
//   - application/json
// produces:
//   - application/json
// parameters:
//   - in: body
//     name: project
//     description: Project rename request
//     required: true
//     schema:
//       $ref: "#/definitions/ProjectPost"
// responses:
//   "200":
//     $ref: "#/responses/EmptySyncResponse"
//   "400":
//     $ref: "#/responses/BadRequest"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func projectPost(d *Daemon, r *http.Request) response.Response {
	name := mux.Vars(r)["name"]

	// Parse the request.
	req := api.ProjectPost{}

	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	// Quick checks.
	if name == projecthelpers.Default {
		return response.Forbidden(fmt.Errorf("The 'default' project cannot be renamed"))
	}

	// Perform the rename.
	run := func(op *operations.Operation) error {
		var id int64
		err := d.cluster.Transaction(func(tx *db.ClusterTx) error {
			project, err := tx.GetProject(req.Name)
			if err != nil && err != db.ErrNoSuchObject {
				return fmt.Errorf("Failed checking if project %q exists: %w", req.Name, err)
			}

			if project != nil {
				return fmt.Errorf("A project named %q already exists", req.Name)
			}

			project, err = tx.GetProject(name)
			if err != nil {
				return fmt.Errorf("Failed loading project %q: %w", name, err)
			}

			if !projectIsEmpty(project) {
				return fmt.Errorf("Only empty projects can be renamed")
			}

			id, err = tx.GetProjectID(name)
			if err != nil {
				return fmt.Errorf("Failed getting project ID for project %q: %w", name, err)
			}

			err = projectValidateName(req.Name)
			if err != nil {
				return err
			}

			return tx.RenameProject(name, req.Name)
		})
		if err != nil {
			return err
		}

		if d.rbac != nil {
			err = d.rbac.RenameProject(id, req.Name)
			if err != nil {
				return err
			}
		}

		requestor := request.CreateRequestor(r)
		d.State().Events.SendLifecycle(req.Name, lifecycle.ProjectRenamed.Event(req.Name, requestor, log.Ctx{"old_name": name}))

		return nil
	}

	op, err := operations.OperationCreate(d.State(), "", operations.OperationClassTask, db.OperationProjectRename, nil, nil, run, nil, nil, r)
	if err != nil {
		return response.InternalError(err)
	}

	return operations.OperationResponse(op)
}

// swagger:operation DELETE /1.0/projects/{name} projects project_delete
//
// Delete the project
//
// Removes the project.
//
// ---
// produces:
//   - application/json
// responses:
//   "200":
//     $ref: "#/responses/EmptySyncResponse"
//   "400":
//     $ref: "#/responses/BadRequest"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func projectDelete(d *Daemon, r *http.Request) response.Response {
	name := mux.Vars(r)["name"]

	// Quick checks.
	if name == projecthelpers.Default {
		return response.Forbidden(fmt.Errorf("The 'default' project cannot be deleted"))
	}

	var id int64
	err := d.cluster.Transaction(func(tx *db.ClusterTx) error {
		project, err := tx.GetProject(name)
		if err != nil {
			return fmt.Errorf("Fetch project %q: %w", name, err)
		}
		if !projectIsEmpty(project) {
			return fmt.Errorf("Only empty projects can be removed")
		}

		id, err = tx.GetProjectID(name)
		if err != nil {
			return fmt.Errorf("Fetch project id %q: %w", name, err)
		}

		return tx.DeleteProject(name)
	})

	if err != nil {
		return response.SmartError(err)
	}

	if d.rbac != nil {
		err = d.rbac.DeleteProject(id)
		if err != nil {
			return response.SmartError(err)
		}
	}

	requestor := request.CreateRequestor(r)
	d.State().Events.SendLifecycle(name, lifecycle.ProjectDeleted.Event(name, requestor, nil))

	return response.EmptySyncResponse
}

// Check if a project is empty.
func projectIsEmpty(project *db.Project) bool {
	if len(project.UsedBy) > 0 {
		// Check if the only entity is the default profile.
		if len(project.UsedBy) == 1 && strings.Contains(project.UsedBy[0], "/profiles/default") {
			return true
		}
		return false
	}
	return true
}

func isEitherAllowOrBlock(value string) error {
	return validate.Optional(validate.IsOneOf("block", "allow"))(value)
}

func isEitherAllowOrBlockOrManaged(value string) error {
	return validate.Optional(validate.IsOneOf("block", "allow", "managed"))(value)
}

func projectValidateConfig(s *state.State, config map[string]string) error {
	// Validate the project configuration.
	projectConfigKeys := map[string]func(value string) error{
		"features.profiles":                    validate.Optional(validate.IsBool),
		"features.images":                      validate.Optional(validate.IsBool),
		"features.storage.volumes":             validate.Optional(validate.IsBool),
		"limits.containers":                    validate.Optional(validate.IsUint32),
		"limits.virtual-machines":              validate.Optional(validate.IsUint32),
		"limits.memory":                        validate.Optional(validate.IsSize),
		"limits.processes":                     validate.Optional(validate.IsUint32),
		"limits.cpu":                           validate.Optional(validate.IsUint32),
		"limits.disk":                          validate.Optional(validate.IsSize),
		"restricted":                           validate.Optional(validate.IsBool),
		"restricted.containers.nesting":        isEitherAllowOrBlock,
		"restricted.containers.lowlevel":       isEitherAllowOrBlock,
		"restricted.containers.privilege":      validate.Optional(validate.IsOneOf("allow", "unprivileged", "isolated")),
		"restricted.virtual-machines.lowlevel": isEitherAllowOrBlock,
		"restricted.devices.unix-char":         isEitherAllowOrBlock,
		"restricted.devices.unix-block":        isEitherAllowOrBlock,
		"restricted.devices.unix-hotplug":      isEitherAllowOrBlock,
		"restricted.devices.infiniband":        isEitherAllowOrBlock,
		"restricted.devices.gpu":               isEitherAllowOrBlock,
		"restricted.devices.usb":               isEitherAllowOrBlock,
		"restricted.devices.pci":               isEitherAllowOrBlock,
		"restricted.devices.proxy":             isEitherAllowOrBlock,
		"restricted.devices.nic":               isEitherAllowOrBlockOrManaged,
		"restricted.devices.disk":              isEitherAllowOrBlockOrManaged,
	}

	for k, v := range config {
		key := k

		// User keys are free for all.
		if strings.HasPrefix(key, "user.") {
			continue
		}

		// Then validate.
		validator, ok := projectConfigKeys[key]
		if !ok {
			return fmt.Errorf("Invalid project configuration key %q", k)
		}

		err := validator(v)
		if err != nil {
			return fmt.Errorf("Invalid project configuration key %q value: %w", k, err)
		}
	}

	// Ensure that restricted projects have their own profiles. Otherwise restrictions in this project could
	// be bypassed by settings from the default project's profiles that are not checked against this project's
	// restrictions when they are configured.
	if shared.IsTrue(config["restricted"]) && shared.IsFalse(config["features.profiles"]) {
		return fmt.Errorf("Projects without their own profiles cannot be restricted")
	}

	return nil
}

func projectValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("No name provided")
	}

	if strings.Contains(name, "/") {
		return fmt.Errorf("Project names may not contain slashes")
	}

	if strings.Contains(name, " ") {
		return fmt.Errorf("Project names may not contain spaces")
	}

	if strings.Contains(name, "_") {
		return fmt.Errorf("Project names may not contain underscores")
	}

	if strings.Contains(name, "'") || strings.Contains(name, `"`) {
		return fmt.Errorf("Project names may not contain quotes")
	}

	if name == "*" {
		return fmt.Errorf("Reserved project name")
	}

	if shared.StringInSlice(name, []string{".", ".."}) {
		return fmt.Errorf("Invalid project name %q", name)
	}

	return nil
}

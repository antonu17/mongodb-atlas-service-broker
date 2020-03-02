package broker

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

	"github.com/mongodb/mongodb-atlas-service-broker/pkg/atlas"
	"github.com/pivotal-cf/brokerapi"
	"github.com/pkg/errors"
)

// ConnectionDetails will be returned when a new binding is created.
type ConnectionDetails struct {
	Username         string `json:"username"`
	Password         string `json:"password"`
	URI              string `json:"uri"`
	ConnectionString string `json:"connection_string"`
}

// ConnectionStringParams will store connectionString parameters from SeviceBinding
type ConnectionStringParams struct {
	SkipCredentials bool                   `json:"skipCredentials,omitempty"`
	Database        string                 `json:"database,omitempty"`
	Options         map[string]interface{} `json:"options,omitempty"`
	Format          string                 `json:"format,omitempty"`
}

// Bind will create a new database user with a username matching the binding ID
// and a randomly generated password. The user credentials will be returned back.
func (b Broker) Bind(ctx context.Context, instanceID string, bindingID string, details brokerapi.BindDetails, asyncAllowed bool) (spec brokerapi.Binding, err error) {
	b.logger.Infow("Creating binding", "instance_id", instanceID, "binding_id", bindingID, "details", details)

	client, err := atlasClientFromContext(ctx)
	if err != nil {
		return
	}

	// The service_id and plan_id are required to be valid per the specification, despite
	// not being used for bindings. We look them up to ensure they can be found in the catalog.
	provider, err := findProviderByServiceID(client, details.ServiceID)
	if err != nil {
		return
	}

	_, err = findInstanceSizeByPlanID(provider, details.PlanID)
	if err != nil {
		return
	}

	// Fetch the cluster from Atlas to ensure it exists.
	cluster, err := client.GetCluster(NormalizeClusterName(instanceID))
	if err != nil {
		b.logger.Errorw("Failed to get existing cluster", "error", err, "instance_id", instanceID)
		err = atlasToAPIError(err)
		return
	}

	// Generate a cryptographically secure random password.
	password, err := generatePassword()
	if err != nil {
		b.logger.Errorw("Failed to generate password", "error", err, "instance_id", instanceID, "binding_id", bindingID)
		err = errors.New("Failed to generate binding password")
		return
	}

	// Construct a cluster definition from the instance ID, service, plan, and params.
	user, err := userFromParams(bindingID, password, details.RawParameters)
	if err != nil {
		b.logger.Errorw("Couldn't create user from the passed parameters", "error", err, "instance_id", instanceID, "binding_id", bindingID, "details", details)
		return
	}

	// Create a new Atlas database user from the generated definition.
	_, err = client.CreateUser(*user)
	if err != nil {
		b.logger.Errorw("Failed to create Atlas database user", "error", err, "instance_id", instanceID, "binding_id", bindingID)
		err = atlasToAPIError(err)
		return
	}

	b.logger.Infow("Successfully created Atlas database user", "instance_id", instanceID, "binding_id", bindingID)

	connectionStringParams, err := connectionStringParamsFromParams(details.RawParameters)
	if err != nil {
		b.logger.Errorw("Couldn't read connection string parameters from the passed parameters", "error", err, "instance_id", instanceID, "binding_id", bindingID, "details", details)
		return
	}
	b.logger.Debugw("Read connectionString from parameters", "connectionStringParams", connectionStringParams)

	connectionString, err := buildConnectionString(connectionStringParams, cluster, bindingID, password)
	if err != nil {
		b.logger.Errorw("Couldn't build connection string URL", "error", err, "instance_id", instanceID, "binding_id", bindingID, "details", details)
		return
	}

	spec = brokerapi.Binding{
		Credentials: ConnectionDetails{
			Username:         bindingID,
			Password:         password,
			URI:              cluster.SrvAddress,
			ConnectionString: connectionString,
		},
	}
	return
}

// Unbind will delete the database user for a specific binding. The database
// user should have the binding ID as its username.
func (b Broker) Unbind(ctx context.Context, instanceID string, bindingID string, details brokerapi.UnbindDetails, asyncAllowed bool) (spec brokerapi.UnbindSpec, err error) {
	b.logger.Infow("Releasing binding", "instance_id", instanceID, "binding_id", bindingID, "details", details)

	client, err := atlasClientFromContext(ctx)
	if err != nil {
		return
	}

	// Fetch the cluster from Atlas to ensure it exists.
	_, err = client.GetCluster(NormalizeClusterName(instanceID))
	if err != nil {
		b.logger.Errorw("Failed to get existing cluster", "error", err, "instance_id", instanceID)
		err = atlasToAPIError(err)
		return
	}

	// Delete database user which has the binding ID as its username.
	err = client.DeleteUser(bindingID)
	if err != nil {
		b.logger.Errorw("Failed to delete Atlas database user", "error", err, "instance_id", instanceID, "binding_id", bindingID)
		err = atlasToAPIError(err)
		return
	}

	b.logger.Infow("Successfully deleted Atlas database user", "instance_id", instanceID, "binding_id", bindingID)

	spec = brokerapi.UnbindSpec{}
	return
}

// GetBinding is currently not supported as specified by the
// BindingsRetrievable setting in the service catalog.
func (b Broker) GetBinding(ctx context.Context, instanceID string, bindingID string) (spec brokerapi.GetBindingSpec, err error) {
	b.logger.Infow("Retrieving binding", "instance_id", instanceID, "binding_id", bindingID)

	err = brokerapi.NewFailureResponse(fmt.Errorf("Unknown binding ID %s", bindingID), 404, "get-binding")
	return
}

// LastBindingOperation should fetch the status of the last creation/deletion
// of a database user.
func (b Broker) LastBindingOperation(ctx context.Context, instanceID string, bindingID string, details brokerapi.PollDetails) (brokerapi.LastOperation, error) {
	panic("not implemented")
}

// generatePassword will generate a cryptographically secure password.
// The password will be base64 encoded for easy usage.
func generatePassword() (string, error) {
	const numberOfBytes = 32
	b := make([]byte, numberOfBytes)

	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}

	return base64.URLEncoding.EncodeToString(b), nil
}

func userFromParams(bindingID string, password string, rawParams []byte) (*atlas.User, error) {
	// Set up a params object which will be used for deserialiation.
	params := struct {
		User *atlas.User `json:"user"`
	}{
		&atlas.User{},
	}

	// If params were passed we unmarshal them into the params object.
	if len(rawParams) > 0 {
		err := json.Unmarshal(rawParams, &params)
		if err != nil {
			return nil, err
		}
	}

	// Set binding ID as username and add password.
	params.User.Username = bindingID
	params.User.Password = password

	// If no role is specified we default to read/write on any database.
	// This is the default role when creating a user through the Atlas UI.
	if len(params.User.Roles) == 0 {
		params.User.Roles = []atlas.Role{
			atlas.Role{
				Name:         "readWriteAnyDatabase",
				DatabaseName: "admin",
			},
		}
	}

	return params.User, nil
}

func connectionStringParamsFromParams(rawParams []byte) (*ConnectionStringParams, error) {
	params := struct {
		ConnectionStringParams *ConnectionStringParams `json:"connectionString"`
	}{
		&ConnectionStringParams{},
	}

	if len(rawParams) > 0 {
		err := json.Unmarshal(rawParams, &params)
		if err != nil {
			return nil, err
		}
	}
	return params.ConnectionStringParams, nil
}

func buildConnectionString(connectionStringParams *ConnectionStringParams, cluster *atlas.Cluster, bindingID, password string) (string, error) {
	var clusterAddress string

	if connectionStringParams.Format == "standard" {
		clusterAddress = cluster.MongoURIWithOptions
	} else {
		clusterAddress = cluster.SrvAddress
	}

	connectionStringURL, err := url.Parse(clusterAddress)
	if err != nil {
		return "", errors.Wrap(err, "Couldn't parse Atlas address URL")
	}

	if !connectionStringParams.SkipCredentials {
		connectionStringURL.User = url.UserPassword(bindingID, password)
	}

	if len(connectionStringParams.Database) > 0 {
		connectionStringURL.Path = connectionStringParams.Database
	}

	if len(connectionStringParams.Options) > 0 {
		q := connectionStringURL.Query()
		for key, value := range connectionStringParams.Options {
			switch v := value.(type) {
			case string:
				q.Set(key, v)
			case float64:
				q.Set(key, strconv.FormatInt(int64(v), 10))
			case bool:
				q.Set(key, strconv.FormatBool(v))
			}
		}
		connectionStringURL.RawQuery = q.Encode()

		if len(connectionStringURL.Path) == 0 {
			connectionStringURL.Path = "/"
		}
	}

	return connectionStringURL.String(), nil
}

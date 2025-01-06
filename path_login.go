// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package cf

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/cloudfoundry-community/go-cfclient"
	"github.com/hashicorp/go-secure-stdlib/strutil"
	"github.com/hashicorp/vault/sdk/framework"
	"github.com/hashicorp/vault/sdk/helper/cidrutil"
	"github.com/hashicorp/vault/sdk/logical"
	"github.com/pkg/errors"

	"github.com/hashicorp/vault-plugin-auth-cf/models"
	"github.com/hashicorp/vault-plugin-auth-cf/signatures"
	"github.com/hashicorp/vault-plugin-auth-cf/util"
)

func (b *backend) pathLogin() *framework.Path {
	return &framework.Path{
		Pattern: "login",
		DisplayAttrs: &framework.DisplayAttributes{
			OperationPrefix: operationPrefixCloudFoundry,
			OperationVerb:   "login",
		},
		Fields: map[string]*framework.FieldSchema{
			"role": {
				Required: true,
				Type:     framework.TypeString,
				DisplayAttrs: &framework.DisplayAttributes{
					Name:  "Role Name",
					Value: "internally-defined-role",
				},
				Description: "The name of the role to authenticate against.",
			},
			"cf_instance_cert": {
				Required: true,
				Type:     framework.TypeString,
				DisplayAttrs: &framework.DisplayAttributes{
					Name: "CF_INSTANCE_CERT Contents",
				},
				Description: "The full body of the file available at the CF_INSTANCE_CERT path on the CF instance.",
			},
			"signing_time": {
				Required: true,
				Type:     framework.TypeString,
				DisplayAttrs: &framework.DisplayAttributes{
					Name:  "Signing Time",
					Value: "2006-01-02T15:04:05Z",
				},
				Description: "The date and time used to construct the signature.",
			},
			"signature": {
				Required: true,
				Type:     framework.TypeString,
				DisplayAttrs: &framework.DisplayAttributes{
					Name: "Signature",
				},
				Description: "The signature generated by the client certificate's private key.",
			},
		},
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.UpdateOperation: &framework.PathOperation{
				Callback: b.operationLoginUpdate,
			},
			logical.ResolveRoleOperation: &framework.PathOperation{
				Callback: b.resolveRole,
			},
		},
		HelpSynopsis:    pathLoginSyn,
		HelpDescription: pathLoginDesc,
	}
}

// resolveRole resolves the role that will be used from this login request.
func (b *backend) resolveRole(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	roleName := data.Get("role").(string)
	if roleName == "" {
		return logical.ErrorResponse("role is required"), nil
	}

	// Ensure the cf certificate meets the role's constraints.
	role, err := getRole(ctx, req.Storage, roleName)
	if err != nil {
		return nil, err
	}
	if role == nil {
		return logical.ErrorResponse(fmt.Sprintf("invalid role name %q", roleName)), nil
	}
	return logical.ResolveRoleResponse(roleName)
}

// operationLoginUpdate is called by those wanting to gain access to Vault.
// They present the instance certificates that should have been issued by the pre-configured
// Certificate Authority, and a signature that should have been signed by the instance cert's
// private key. If this holds true, there are additional checks verifying everything looks
// good before authentication is given.
func (b *backend) operationLoginUpdate(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	// Grab the time immediately for checking against the request's signingTime.
	timeReceived := time.Now().UTC()

	roleName := data.Get("role").(string)
	if roleName == "" {
		return logical.ErrorResponse("'role-name' is required"), nil
	}

	// Ensure the cf certificate meets the role's constraints.
	role, err := getRole(ctx, req.Storage, roleName)
	if err != nil {
		return nil, err
	}
	if role == nil {
		return nil, errors.New("no matching role")
	}

	if len(role.TokenBoundCIDRs) > 0 {
		if req.Connection == nil {
			b.Logger().Warn("token bound CIDRs found but no connection information available for validation")
			return nil, logical.ErrPermissionDenied
		}
		if !cidrutil.RemoteAddrIsOk(req.Connection.RemoteAddr, role.TokenBoundCIDRs) {
			return nil, logical.ErrPermissionDenied
		}
	}

	signature := data.Get("signature").(string)
	if signature == "" {
		return logical.ErrorResponse("'signature' is required"), nil
	}

	cfInstanceCertContents := data.Get("cf_instance_cert").(string)
	if cfInstanceCertContents == "" {
		return logical.ErrorResponse("'cf_instance_cert' is required"), nil
	}

	signingTimeRaw := data.Get("signing_time").(string)
	if signingTimeRaw == "" {
		return logical.ErrorResponse("'signing_time' is required"), nil
	}
	signingTime, err := parseTime(signingTimeRaw)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	b.mu.RLock()
	defer b.mu.RUnlock()
	config, err := getConfig(ctx, req.Storage)
	if err != nil {
		return nil, err
	}
	if config == nil {
		return nil, errors.New("no CA is configured for verifying client certificates")
	}

	// Ensure the time it was signed isn't too far in the past or future.
	oldestAllowableSigningTime := timeReceived.Add(-1 * config.LoginMaxSecNotBefore)
	furthestFutureAllowableSigningTime := timeReceived.Add(config.LoginMaxSecNotAfter)
	if signingTime.Before(oldestAllowableSigningTime) {
		return logical.ErrorResponse(fmt.Sprintf("request is too old; signed at %s but received request at %s; allowable seconds old is %d", signingTime, timeReceived, config.LoginMaxSecNotBefore/time.Second)), nil
	}
	if signingTime.After(furthestFutureAllowableSigningTime) {
		return logical.ErrorResponse(fmt.Sprintf("request is too far in the future; signed at %s but received request at %s; allowable seconds in the future is %d", signingTime, timeReceived, config.LoginMaxSecNotAfter/time.Second)), nil
	}

	intermediateCert, identityCert, err := util.ExtractCertificates(cfInstanceCertContents)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	// Ensure the private key used to create the signature matches our identity
	// certificate, and that it signed the same data as is presented in the body.
	// This offers some protection against MITM attacks.
	signingCert, err := signatures.Verify(signature, &signatures.SignatureData{
		SigningTime:            signingTime,
		Role:                   roleName,
		CFInstanceCertContents: cfInstanceCertContents,
	})
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	// Make sure the identity/signing cert was actually issued by our CA.
	if err := util.Validate(config.IdentityCACertificates, intermediateCert, identityCert, signingCert); err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	// Read CF's identity fields from the certificate.
	cfCert, err := models.NewCFCertificateFromx509(signingCert)
	if err != nil {
		return nil, err
	}

	// It may help some users to be able to easily view the incoming certificate information
	// in an un-encoded format, as opposed to the encoded format that will appear in the Vault
	// audit logs.
	if b.Logger().IsDebug() {
		b.Logger().Debug(fmt.Sprintf("handling login attempt from %+v", cfCert))
	}

	client, err := b.getCFClientOrRefresh(ctx, config)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	if err := b.validate(client, role, cfCert, req.Connection.RemoteAddr); err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	orgName, err := b.getOrgName(client, cfCert)
	if err != nil {
		return nil, err
	}

	appName, err := b.getAppName(client, cfCert)
	if err != nil {
		return nil, err
	}

	spaceName, err := b.getSpaceName(client, cfCert)
	if err != nil {
		return nil, err
	}

	// Everything checks out.
	auth := &logical.Auth{
		InternalData: map[string]interface{}{
			"role":        roleName,
			"instance_id": cfCert.InstanceID,
			"ip_address":  cfCert.IPAddress,
		},
		DisplayName: cfCert.InstanceID,
		Alias: &logical.Alias{
			Name: cfCert.AppID,
			Metadata: map[string]string{
				"org_id":     cfCert.OrgID,
				"app_id":     cfCert.AppID,
				"space_id":   cfCert.SpaceID,
				"org_name":   orgName,
				"app_name":   appName,
				"space_name": spaceName,
			},
		},
	}

	role.PopulateTokenAuth(auth)

	return &logical.Response{
		Auth: auth,
	}, nil
}

func (b *backend) pathLoginRenew(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	config, err := getConfig(ctx, req.Storage)
	if err != nil {
		return nil, err
	}
	if config == nil {
		return nil, errors.New("no configuration is available for reaching the CF API")
	}

	roleName, err := getOrErr("role", req.Auth.InternalData)
	if err != nil {
		return nil, err
	}

	role, err := getRole(ctx, req.Storage, roleName)
	if err != nil {
		return nil, err
	}
	if role == nil {
		return nil, errors.New("no matching role")
	}

	instanceID, err := getOrErr("instance_id", req.Auth.InternalData)
	if err != nil {
		return nil, err
	}

	ipAddr, err := getOrErr("ip_address", req.Auth.InternalData)
	if err != nil {
		return nil, err
	}

	orgID, err := getOrErr("org_id", req.Auth.Alias.Metadata)
	if err != nil {
		return nil, err
	}

	spaceID, err := getOrErr("space_id", req.Auth.Alias.Metadata)
	if err != nil {
		return nil, err
	}

	appID, err := getOrErr("app_id", req.Auth.Alias.Metadata)
	if err != nil {
		return nil, err
	}

	// Reconstruct the certificate and ensure it still meets all constraints.
	cfCert, err := models.NewCFCertificate(instanceID, orgID, spaceID, appID, ipAddr)

	client, err := b.getCFClientOrRefresh(ctx, config)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	if err := b.validate(client, role, cfCert, req.Connection.RemoteAddr); err != nil {
		// taint the client on error so that it will be refreshed on the next login attempt
		b.cfClientTainted = true
		return logical.ErrorResponse(err.Error()), nil
	}

	resp := &logical.Response{Auth: req.Auth}
	resp.Auth.TTL = role.TokenTTL
	resp.Auth.MaxTTL = role.TokenMaxTTL
	resp.Auth.Period = role.TokenPeriod
	return resp, nil
}

func (b *backend) validate(client *cfclient.Client, role *models.RoleEntry, cfCert *models.CFCertificate, reqConnRemoteAddr string) error {
	if !role.DisableIPMatching {
		if !matchesIPAddress(reqConnRemoteAddr, net.ParseIP(cfCert.IPAddress)) {
			return errors.New("no matching IP address")
		}
	}
	if !meetsBoundConstraints(cfCert.InstanceID, role.BoundInstanceIDs) {
		return fmt.Errorf("instance ID %s doesn't match role constraints of %s", cfCert.InstanceID, role.BoundInstanceIDs)
	}
	if !meetsBoundConstraints(cfCert.AppID, role.BoundAppIDs) {
		return fmt.Errorf("app ID %s doesn't match role constraints of %s", cfCert.AppID, role.BoundAppIDs)
	}
	if !meetsBoundConstraints(cfCert.OrgID, role.BoundOrgIDs) {
		return fmt.Errorf("org ID %s doesn't match role constraints of %s", cfCert.OrgID, role.BoundOrgIDs)
	}
	if !meetsBoundConstraints(cfCert.SpaceID, role.BoundSpaceIDs) {
		return fmt.Errorf("space ID %s doesn't match role constraints of %s", cfCert.SpaceID, role.BoundSpaceIDs)
	}
	// Use the CF API to ensure everything still exists and to verify whatever we can.

	// Here, if it were possible, we _would_ do an API call to check the instance ID,
	// but currently there's no known way to do that via the cf API.

	// Check everything we can using the app ID.
	app, err := client.AppByGuid(cfCert.AppID)
	if err != nil {
		return err
	}
	if app.Guid != cfCert.AppID {
		return fmt.Errorf("cert app ID %s doesn't match API's expected one of %s", cfCert.AppID, app.Guid)
	}
	if app.SpaceGuid != cfCert.SpaceID {
		return fmt.Errorf("cert space ID %s doesn't match API's expected one of %s", cfCert.SpaceID, app.SpaceGuid)
	}
	if app.Instances <= 0 {
		return errors.New("app doesn't have any live instances")
	}

	// Check everything we can using the org ID.
	org, err := client.GetOrgByGuid(cfCert.OrgID)
	if err != nil {
		return err
	}
	if org.Guid != cfCert.OrgID {
		return fmt.Errorf("cert org ID %s doesn't match API's expected one of %s", cfCert.OrgID, org.Guid)
	}

	// Check everything we can using the space ID.
	space, err := client.GetSpaceByGuid(cfCert.SpaceID)
	if err != nil {
		return err
	}
	if space.Guid != cfCert.SpaceID {
		return fmt.Errorf("cert space ID %s doesn't match API's expected one of %s", cfCert.SpaceID, space.Guid)
	}
	if space.OrganizationGuid != cfCert.OrgID {
		return fmt.Errorf("cert org ID %s doesn't match API's expected one of %s", cfCert.OrgID, space.OrganizationGuid)
	}
	return nil
}

func (b *backend) getOrgName(client *cfclient.Client, cfCert *models.CFCertificate) (string, error) {
	org, err := client.GetOrgByGuid(cfCert.OrgID)
	if err != nil {
		return "", err
	}

	return org.Name, nil
}

func (b *backend) getAppName(client *cfclient.Client, cfCert *models.CFCertificate) (string, error) {
	app, err := client.AppByGuid(cfCert.AppID)
	if err != nil {
		return "", err
	}

	return app.Name, nil
}

func (b *backend) getSpaceName(client *cfclient.Client, cfCert *models.CFCertificate) (string, error) {
	space, err := client.GetSpaceByGuid(cfCert.SpaceID)
	if err != nil {
		return "", err
	}

	return space.Name, nil
}

func meetsBoundConstraints(certValue string, constraints []string) bool {
	if len(constraints) == 0 {
		// There are no restrictions, so everything passes this check.
		return true
	}
	// Check whether we have a match.
	return strutil.StrListContains(constraints, certValue)
}

func matchesIPAddress(remoteAddr string, certIP net.IP) bool {
	// Some remote addresses may arrive like "10.255.181.105/32"
	// but the certificate will only have the IP address without
	// the subnet mask, so that's what we want to match against.
	// For those wanting to also match the subnet, use bound_cidrs.
	parts := strings.Split(remoteAddr, "/")
	reqIPAddr := net.ParseIP(parts[0])
	if certIP.Equal(reqIPAddr) {
		return true
	}
	return false
}

// Try parsing this as ISO 8601 AND the way that is default provided by Bash to make it easier to give via the CLI as well.
func parseTime(signingTime string) (time.Time, error) {
	if signingTime, err := time.Parse(signatures.TimeFormat, signingTime); err == nil {
		return signingTime, nil
	}
	if signingTime, err := time.Parse(util.BashTimeFormat, signingTime); err == nil {
		return signingTime, nil
	}
	return time.Time{}, fmt.Errorf("couldn't parse %s", signingTime)
}

// getOrErr is a convenience method for pulling a string from a map.
func getOrErr(fieldName string, from interface{}) (string, error) {
	switch givenMap := from.(type) {
	case map[string]interface{}:
		vIfc, ok := givenMap[fieldName]
		if !ok {
			return "", fmt.Errorf("unable to retrieve %q during renewal", fieldName)
		}
		v, ok := vIfc.(string)
		if v == "" {
			return "", fmt.Errorf("unable to retrieve %q during renewal, not a string", fieldName)
		}
		return v, nil
	case map[string]string:
		v, ok := givenMap[fieldName]
		if !ok {
			return "", fmt.Errorf("unable to retrieve %q during renewal", fieldName)
		}
		return v, nil
	default:
		return "", fmt.Errorf("unrecognized type for structure containing %s", fieldName)
	}
}

const pathLoginSyn = `
Authenticates an entity with Vault.
`

const pathLoginDesc = `
Authenticate CF entities using a client certificate issued by the 
configured Certificate Authority, and signed by a client key belonging
to the client certificate.
`

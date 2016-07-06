package auth

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"

	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/errors"
	"golang.org/x/crypto/openpgp/packet"
	"gopkg.in/yaml.v2"

	"github.com/square/p2/pkg/logging"
	"github.com/square/p2/pkg/types"
	"github.com/square/p2/pkg/util"
)

// These string constants are used to determine the requested auth policy type
// both in the "auth" section of preparer config and in p2-launch flags
const (
	Null    = "none"
	Keyring = "keyring"
	User    = "user"
)

// A Policy encapsulates the behavior a p2 node needs to authorize
// its actions. It is possible for implementations to rely on other
// services for these behaviors, so these calls may be slow or
// transiently fail.
type Policy interface {
	// Check if an App is authorized to be installed and run on this
	// node. This involves checking that the app's pod manifest has a
	// valid signature and that the signer is authorized to
	// install/run the app. If the action is authorized, `nil` will be
	// returned.
	AuthorizeApp(manifest Manifest, logger logging.Logger) error

	// Check if a hook is authorized to be used on this node. A hook
	// is distributed as a pod, but they are extensions of P2 itself,
	// so they are treated differently than user-deployed Apps. If the
	// action is authorized, `nil` will be returned.
	AuthorizeHook(manifest Manifest, logger logging.Logger) error

	// Check if a file digest has a valid signature and that the
	// signer is authorized to certify the digest. The caller must
	// separately check that the actual files match the digest. If
	// the action is authorized, `nil` will be returned.
	CheckDigest(digest Digest) error

	// Release any resources held by the policy implementation.
	Close()
}

// auth.Manifest mirrors manifest.Manifest, listing only the data
// accessors that auth logic cares about.
type Manifest interface {
	ID() types.PodID
	RunAsUser() string
	Signed
}

// auth.Digest contains all info needed to certify a digest over the
// files in a launchable.
type Digest interface {
	Signed
	// No other data examined at the moment
}

// A Signed object contains some plaintext encoding and a signature
// that data.
type Signed interface {
	// Return plaintext and signature data.  If there is no plaintext
	// or signature, use `nil`.
	SignatureData() (plaintext, signature []byte)
}

// auth.Error wraps all errors generated by the authorization layer,
// allowing errors to carry structured data.
type Error struct {
	Err    error
	Fields map[string]interface{} // Context for structured logging
}

func (e Error) Error() string {
	return e.Err.Error()
}

// The NullPolicy never disallows anything. Everything is safe!
type NullPolicy struct{}

func (p NullPolicy) AuthorizeApp(manifest Manifest, logger logging.Logger) error {
	return nil
}

func (p NullPolicy) AuthorizeHook(manifest Manifest, logger logging.Logger) error {
	return nil
}

func (p NullPolicy) CheckDigest(digest Digest) error {
	return nil
}

func (p NullPolicy) Close() {
}

// Assert that NullPolicy is a Policy
var _ Policy = NullPolicy{}

// The FixedKeyring policy holds one keyring. A pod is authorized to be
// deployed iff:
// 1. The manifest is signed by a key on the keyring, and
// 2. If the pod ID has an authorization list, the signing key is on
//    the list.
//
// Artifacts can optionally sign their contents. If no digest
// signature is provided, the deployment is authorized. If a signature
// exists, deployment is authorized iff the signer is on the keyring.
type FixedKeyringPolicy struct {
	Keyring             openpgp.KeyRing
	AuthorizedDeployers map[types.PodID][]string
}

func LoadKeyringPolicy(
	keyringPath string,
	authorizedDeployers map[types.PodID][]string,
) (Policy, error) {
	keyring, err := LoadKeyring(keyringPath)
	if err != nil {
		return nil, err
	}
	return FixedKeyringPolicy{keyring, authorizedDeployers}, nil
}

func (p FixedKeyringPolicy) AuthorizeApp(manifest Manifest, logger logging.Logger) error {
	plaintext, signature := manifest.SignatureData()
	if signature == nil {
		return Error{util.Errorf("received unsigned manifest (expected signature)"), nil}
	}
	signer, err := checkDetachedSignature(p.Keyring, plaintext, signature)
	if err != nil {
		return err
	}

	signerId := fmt.Sprintf("%X", signer.PrimaryKey.Fingerprint)
	logger.WithField("signer_key", signerId).Debugln("resolved manifest signature")

	// Check authorization for this package to be deployed by this
	// key, if configured.
	if len(p.AuthorizedDeployers[manifest.ID()]) > 0 {
		found := false
		for _, deployerId := range p.AuthorizedDeployers[manifest.ID()] {
			if deployerId == signerId {
				found = true
				break
			}
		}
		if !found {
			return Error{
				util.Errorf("manifest signer not authorized to deploy " + string(manifest.ID())),
				map[string]interface{}{"signer_key": signerId},
			}
		}
	}

	return nil
}

func (p FixedKeyringPolicy) AuthorizeHook(manifest Manifest, logger logging.Logger) error {
	return p.AuthorizeApp(manifest, logger)
}

func (p FixedKeyringPolicy) CheckDigest(digest Digest) error {
	plaintext, signature := digest.SignatureData()
	if signature == nil {
		return nil
	}
	_, err := checkDetachedSignature(p.Keyring, plaintext, signature)
	return err
}

func (p FixedKeyringPolicy) Close() {
}

// Returns the key ID used to sign a message. This method is extracted
// from `openpgp.CheckDetachedSignature()`, which only reports that a
// key wasn't found, not *which* key wasn't found.
func signerKeyId(signature []byte) (uint64, error) {
	p, err := packet.Read(bytes.NewReader(signature))
	if err != nil {
		return 0, err
	}
	switch sig := p.(type) {
	case *packet.Signature:
		if sig.IssuerKeyId == nil {
			return 0, errors.StructuralError("signature doesn't have an issuer")
		}
		return *sig.IssuerKeyId, nil
	case *packet.SignatureV3:
		return sig.IssuerKeyId, nil
	default:
		return 0, errors.StructuralError("non signature packet found")
	}
}

// Wrapper around openpgp.CheckDetachedSignature() that standardizes
// the error messages.
func checkDetachedSignature(
	keyring openpgp.KeyRing,
	signed []byte,
	signature []byte,
) (*openpgp.Entity, error) {
	signer, err := openpgp.CheckDetachedSignature(
		keyring,
		bytes.NewReader(signed),
		bytes.NewReader(signature),
	)
	if err == errors.ErrUnknownIssuer {
		keyId, err := signerKeyId(signature)
		if err != nil {
			return nil, Error{util.Errorf("error validating signature: %s", err), nil}
		}
		return nil, Error{util.Errorf("unknown signer: %X", keyId), nil}
	}
	if err != nil {
		return nil, Error{util.Errorf("error validating signature: %s", err), nil}
	}
	return signer, nil
}

func LoadKeyring(path string) (openpgp.EntityList, error) {
	if path == "" {
		return nil, util.Errorf("no keyring configured")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Accept both ASCII-armored and binary encodings
	keyring, err := openpgp.ReadArmoredKeyRing(f)
	if err != nil && err.Error() == "openpgp: invalid argument: no armored data found" {
		offset, seekErr := f.Seek(0, os.SEEK_SET)
		if offset != 0 || seekErr != nil {
			return nil, util.Errorf(
				"couldn't seek to beginning, got %d %s",
				offset,
				seekErr,
			)
		}
		keyring, err = openpgp.ReadKeyRing(f)
	}

	return keyring, err
}

// Assert that FixedKeyringPolicy is a Policy
var _ Policy = FixedKeyringPolicy{}

// FileKeyringPolicy has the same authorization policy as
// FixedKeyringPolicy, but it always pulls its keyring from a file on
// disk. Whenever the keyring is needed, the file is reloaded if it
// has changed since the last time it was read (determined by
// examining mtime).
type FileKeyringPolicy struct {
	KeyringFilename     string
	AuthorizedDeployers map[types.PodID][]string
	keyringWatcher      util.FileWatcher
}

func NewFileKeyringPolicy(
	keyringPath string,
	authorizedDeployers map[types.PodID][]string,
) (Policy, error) {
	watcher, err := util.NewFileWatcher(
		func(path string) (interface{}, error) {
			return LoadKeyring(path)
		},
		keyringPath,
	)
	if err != nil {
		return nil, err
	}
	return FileKeyringPolicy{keyringPath, authorizedDeployers, watcher}, nil
}

func (p FileKeyringPolicy) AuthorizeApp(manifest Manifest, logger logging.Logger) error {
	return FixedKeyringPolicy{
		(<-p.keyringWatcher.GetAsync()).(openpgp.EntityList),
		p.AuthorizedDeployers,
	}.AuthorizeApp(manifest, logger)
}

func (p FileKeyringPolicy) AuthorizeHook(manifest Manifest, logger logging.Logger) error {
	return p.AuthorizeApp(manifest, logger)
}

func (p FileKeyringPolicy) CheckDigest(digest Digest) error {
	return FixedKeyringPolicy{
		(<-p.keyringWatcher.GetAsync()).(openpgp.EntityList),
		p.AuthorizedDeployers,
	}.CheckDigest(digest)
}

func (p FileKeyringPolicy) Close() {
	p.keyringWatcher.Close()
}

// Assert that FileKeyringPolicy is a Policy
var _ Policy = FileKeyringPolicy{}

// A DeployPol lists all app users that apps can run as and the set of
// users (humans or other apps) that are allowed to deploy to each app
// user. (This is the deploy policy, but it isn't a `Policy`
// interface, so the name is shortened.)
//
// This policy is applicable when an app's security policy is centered
// around the Unix user that the app runs as. Similar to "sudo",
// authorization grants one user the ability to run commands (apps) as
// another user.
//
// The policy file should be a YAML-serialized object that conforms to
// the layout of the `RawDeployPol` type. The data in the "groups" key
// is a map: each entry's key defines a group's name and its value
// defines the email addresses in the group. The name of the group is
// not significant. The data in the "apps" key is also a map: each
// entry's key is the name of an app user, and its value is a list of
// groups, each member of which is authorized to deploy apps that will
// run as the app user.
//
// By separating apps from groups, it allows some flexibility in
// managing the deployers for an app. Some possible organizations are:
//
// - Have one group that includes all deployers, and each app user
//   references that group
// - Create one group for each app user, explicitly listing all
//   deployers for that app
// - Create groups for each team, and let each app user be deployed by
//   the team that develops/manages it
//
// Example policy file:
//   ---
//   groups:
//     teamA:
//     - alice@my.org
//     - bob@my.org
//     admins:
//     - carol@my.org
//   apps:
//     web:
//     - teamA
//     - admins
//     db:
//     - admins
//
// In this example, "alice" is authorized to deploy a pod named "api"
// that runs as the Unix user "web". Alice cannot however deploy the
// pod named "mysql" which runs as the "db" user.  Note that Alice
// *is* permitted to deploy "mysql" running as the user "web"--it is
// beyond the scope of this policy to ensure that only the "db" user
// is actually capable of serving database traffic.
type DeployPol struct {
	Groups map[DpGroup]map[DpUserEmail]bool // Each group is a *set* of email addrs
	Apps   map[string][]DpGroup             // Each app has a list of authorized groups
}

// Specialized types make code self-documenting
type DpGroup string
type DpUserEmail string

type RawDeployPol struct {
	Groups map[DpGroup][]DpUserEmail
	Apps   map[string][]DpGroup
}

// Load a new DeployPol from a file.
func LoadDeployPol(filename string) (DeployPol, error) {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return DeployPol{}, err
	}
	var rawDp RawDeployPol
	err = yaml.Unmarshal(data, &rawDp)
	if err != nil {
		return DeployPol{}, err
	}
	// Pre-process *lists of users* into *sets of users*
	groups := make(map[DpGroup]map[DpUserEmail]bool)
	for group, userList := range rawDp.Groups {
		userSet := make(map[DpUserEmail]bool)
		for _, user := range userList {
			userSet[user] = true
		}
		groups[group] = userSet
	}
	return DeployPol{groups, rawDp.Apps}, nil
}

// Check if given user is authorized to act as the given app user. The
// default policy is to fail closed if no app user is found.
func (dp DeployPol) Authorized(appUser string, email string) bool {
	for _, group := range dp.Apps[appUser] {
		if dp.Groups[group][DpUserEmail(email)] {
			return true
		}
	}
	return false
}

// UserPolicy is a Policy that authorizes users' actions instead of
// simply checking for the presence of a key. Users are identified by
// the email addresses associated with their signing key. An external
// policy defines every app user and which email addresses are allowed
// to act as that app user.
//
// The P2 preparer has special authorization check: apps with the
// preparer's name are checked with a different effective app
// user. Hooks, being extensions of the preparer itself, are always
// authorized the same as the preparer.
//
// The deploy policy file should be a YAML file as specified in the
// comments for the `DeployPol` type. The given keyring file should
// contain PGP keys with email addresses that match the emails in the
// deploy policy.  The keyring used by this policy *must be validated*
// to ensure that each key contains correct email addresses.
type UserPolicy struct {
	keyringWatcher util.FileWatcher
	deployWatcher  util.FileWatcher
	preparerApp    types.PodID
	preparerUser   string
}

var _ Policy = UserPolicy{}

func NewUserPolicy(
	keyringPath string,
	deployPolicyPath string,
	preparerApp types.PodID,
	preparerUser string,
) (p Policy, err error) {
	keyringWatcher, err := util.NewFileWatcher(
		func(path string) (interface{}, error) {
			return LoadKeyring(path)
		},
		keyringPath,
	)
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			keyringWatcher.Close()
		}
	}()
	deployWatcher, err := util.NewFileWatcher(
		func(path string) (interface{}, error) {
			return LoadDeployPol(path)
		},
		deployPolicyPath,
	)
	if err != nil {
		return
	}
	p = UserPolicy{keyringWatcher, deployWatcher, preparerApp, preparerUser}
	return
}

func (p UserPolicy) AuthorizePod(podUser string, manifest Signed, logger logging.Logger) error {
	// Verify that the signature is valid
	plaintext, signature := manifest.SignatureData()
	if signature == nil {
		return Error{util.Errorf("received unsigned manifest"), nil}
	}
	keyringChan := p.keyringWatcher.GetAsync()
	dpolChan := p.deployWatcher.GetAsync()
	keyring := (<-keyringChan).(openpgp.EntityList)
	dpol := (<-dpolChan).(DeployPol)

	signer, err := checkDetachedSignature(keyring, plaintext, signature)
	if err != nil {
		return err
	}

	// Check if any of the signer's identities is authorized
	lastIdName := "(unknown)"
	for name, id := range signer.Identities {
		if dpol.Authorized(podUser, id.UserId.Email) {
			return nil
		}
		lastIdName = name
	}
	return Error{util.Errorf("user not authorized to deploy app: %s", lastIdName), nil}
}

func (p UserPolicy) AuthorizeApp(manifest Manifest, logger logging.Logger) error {
	user := manifest.RunAsUser()
	if manifest.ID() == p.preparerApp {
		user = p.preparerUser
	}
	return p.AuthorizePod(user, manifest, logger)
}

func (p UserPolicy) AuthorizeHook(manifest Manifest, logger logging.Logger) error {
	return p.AuthorizePod(p.preparerUser, manifest, logger)
}

func (p UserPolicy) CheckDigest(digest Digest) error {
	return FixedKeyringPolicy{
		(<-p.keyringWatcher.GetAsync()).(openpgp.EntityList),
		nil,
	}.CheckDigest(digest)
}

func (p UserPolicy) Close() {
	p.keyringWatcher.Close()
	p.deployWatcher.Close()
}
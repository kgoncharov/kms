/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package kms

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/cucumber/godog"
	"github.com/hyperledger/aries-framework-go/component/storageutil/mem"
	"github.com/hyperledger/aries-framework-go/pkg/crypto"
	"github.com/hyperledger/aries-framework-go/pkg/doc/ld"
	"github.com/hyperledger/aries-framework-go/pkg/doc/signature/jsonld"
	"github.com/hyperledger/aries-framework-go/pkg/doc/signature/suite"
	"github.com/hyperledger/aries-framework-go/pkg/doc/signature/suite/ed25519signature2018"
	"github.com/hyperledger/aries-framework-go/pkg/kms"
	ldstore "github.com/hyperledger/aries-framework-go/pkg/store/ld"
	"github.com/hyperledger/aries-framework-go/spi/storage"
	"github.com/igor-pavlenko/httpsignatures-go"
	"github.com/trustbloc/edge-core/pkg/log"
	"github.com/trustbloc/edge-core/pkg/zcapld"

	zcapsvc "github.com/trustbloc/kms/pkg/zcapld"
	bddcontext "github.com/trustbloc/kms/test/bdd/pkg/context"
	"github.com/trustbloc/kms/test/bdd/pkg/internal/cryptoutil"
)

const (
	createKeystoreEndpoint = "/v1/keystores"
	createDIDEndpoint      = "/v1/keystores/did"
	keysEndpoint           = "/v1/keystores/{keystoreID}/keys"
	exportKeyEndpoint      = "/v1/keystores/{keystoreID}/keys/{keyID}/export"
	signEndpoint           = "/v1/keystores/{keystoreID}/keys/{keyID}/sign"
	verifyEndpoint         = "/v1/keystores/{keystoreID}/keys/{keyID}/verify"
)

// Steps defines steps context for the KMS operations.
type Steps struct {
	bddContext *bddcontext.BDDContext
	httpClient *http.Client
	logger     log.Logger
	users      map[string]*user
	keys       map[string][]byte
}

// NewSteps creates steps context for the KMS operations.
func NewSteps(tlsConfig *tls.Config) *Steps {
	return &Steps{
		httpClient: &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}},
		logger:     log.New("kms/tests/kms"),
		users:      map[string]*user{},
		keys:       map[string][]byte{"testCEK": cryptoutil.GenerateKey()},
	}
}

// SetContext sets a fresh context for every scenario.
func (s *Steps) SetContext(ctx *bddcontext.BDDContext) {
	s.bddContext = ctx
}

// RegisterSteps defines scenario steps.
func (s *Steps) RegisterSteps(ctx *godog.ScenarioContext) {
	// common creation steps
	ctx.Step(`^Create "([^"]*)" users$`, s.createUsers)
	ctx.Step(`^Create "([^"]*)" users from prototype "([^"]*)"$`, s.createUsersFromPrototype)
	ctx.Step(`^"([^"]*)" login with "([^"]*)" and gets "([^"]*)" and "([^"]*)" env$`, s.stressTestLogin)
	ctx.Step(`^"([^"]*)" wallet has stored secret on Hub Auth$`, s.storeSecretInHubAuth)
	ctx.Step(`^"([^"]*)" has created a data vault on EDV for storing keys$`, s.createEDVDataVault)
	ctx.Step(`^"([^"]*)" users has created a data vault on EDV for storing keys$`, s.createEDVDataVaultForMultipleUsers)
	ctx.Step(`^"([^"]*)" has created an empty keystore on Key Server$`, s.createKeystore)
	ctx.Step(`^"([^"]*)" has created a keystore with "([^"]*)" key on Key Server$`, s.createKeystoreAndKey)
	ctx.Step(`^"([^"]*)" users request to create a keystore on "([^"]*)" with "([^"]*)" key and sign ([^"]*) times using "([^"]*)" concurrent requests$`, //nolint:lll
		s.stressTestForMultipleUsers)

	ctx.Step(`^"([^"]*)" requests to authz kms to create a keystore and a key for user "([^"]*)" and sign using "([^"]*)" concurrent requests$`, //nolint:lll
		s.authStressTestForMultipleUsers)

	// common response checking steps
	ctx.Step(`^"([^"]*)" gets a response with HTTP status "([^"]*)"$`, s.checkRespStatus)
	ctx.Step(`^"([^"]*)" gets a response with HTTP status "([^"]*)" for each request$`, s.checkMultiRespStatus)
	ctx.Step(`^"([^"]*)" gets a response with "([^"]*)" header with a valid URL$`, s.checkHeaderWithValidURL)
	ctx.Step(`^"([^"]*)" gets a response with non-empty "([^"]*)"$`, s.checkRespWithNonEmptyValue)
	ctx.Step(`^"([^"]*)" gets a response with no "([^"]*)"$`, s.checkRespWithNoValue)
	ctx.Step(`^"([^"]*)" gets a response with "([^"]*)" with value "([^"]*)"$`, s.checkRespWithValue)
	ctx.Step(`^"([^"]*)" gets a response with content of "([^"]*)" key$`, s.checkRespWithKeyContent)
	// create/export/import key steps
	ctx.Step(`^"([^"]*)" makes an HTTP POST to "([^"]*)" to create "([^"]*)" key$`, s.makeCreateKeyReq)
	ctx.Step(`^"([^"]*)" makes parallel HTTP POST requests to "([^"]*)" to create "([^"]*)" keys$`,
		s.makeParallelCreateKeyReqs)
	ctx.Step(`^"([^"]*)" makes an HTTP GET to "([^"]*)" to export public key$`, s.makeExportPubKeyReq)
	ctx.Step(`^"([^"]*)" makes an HTTP POST to "([^"]*)" to create and export "([^"]*)" key$`,
		s.makeCreateAndExportKeyReq)
	ctx.Step(`^"([^"]*)" makes an HTTP PUT to "([^"]*)" to import a private key with ID "([^"]*)"$`,
		s.makeImportKeyReq)
	ctx.Step(`^"([^"]*)" makes an HTTP POST to "([^"]*)" to rotate "([^"]*)" key$`, s.makeRotateKeyReq)
	// sign/verify message steps
	ctx.Step(`^"([^"]*)" makes an HTTP POST to "([^"]*)" to sign "([^"]*)"$`, s.makeSignMessageReq)
	ctx.Step(`^"([^"]*)" makes an HTTP POST to "([^"]*)" to verify "([^"]*)" for "([^"]*)"$`, s.makeVerifySignatureReq)

	// encrypt/decrypt message steps
	ctx.Step(`^"([^"]*)" makes an HTTP POST to "([^"]*)" to encrypt "([^"]*)"$`, s.makeEncryptMessageReq)
	ctx.Step(`^"([^"]*)" makes an HTTP POST to "([^"]*)" to decrypt "([^"]*)"$`, s.makeDecryptCipherReq)
	// compute/verify MAC steps
	ctx.Step(`^"([^"]*)" makes an HTTP POST to "([^"]*)" to compute MAC for "([^"]*)"$`, s.makeComputeMACReq)
	ctx.Step(`^"([^"]*)" makes an HTTP POST to "([^"]*)" to verify MAC "([^"]*)" for "([^"]*)"$`, s.makeVerifyMACReq)
	// wrap/unwrap key steps
	ctx.Step(`^"([^"]*)" has a public key of "([^"]*)"$`, s.getPubKeyOfRecipient)
	ctx.Step(`^"([^"]*)" makes an HTTP POST to "([^"]*)" to wrap "([^"]*)" for "([^"]*)"$`, s.makeWrapKeyReq)
	ctx.Step(`^"([^"]*)" makes an HTTP POST to "([^"]*)" to unwrap "([^"]*)" from "([^"]*)"$`, s.makeUnwrapKeyReq)
	// CryptoBox steps
	ctx.Step(`^"([^"]*)" makes an HTTP POST to "([^"]*)" to easy "([^"]*)" for "([^"]*)"$`, s.makeEasyPayloadReq)
	ctx.Step(`^"([^"]*)" makes an HTTP POST to "([^"]*)" to easyOpen "([^"]*)" from "([^"]*)"$`, s.makeEasyOpenReq)
	ctx.Step(`^"([^"]*)" has sealed "([^"]*)" for "([^"]*)"$`, s.sealPayloadForRecipient)
	ctx.Step(`^"([^"]*)" makes an HTTP POST to "([^"]*)" to sealOpen "([^"]*)" from "([^"]*)"$`, s.makeSealOpenReq)
}

func (s *Steps) createKeystoreAndKey(user, keyType string) error {
	if err := s.createKeystore(user); err != nil {
		return err
	}

	return s.makeCreateKeyReq(user, s.bddContext.KeyServerURL+keysEndpoint, keyType)
}

func (s *Steps) createKeystore(userName string) error {
	u := s.users[userName]

	if err := s.createDID(u); err != nil {
		return err
	}

	edvCapability, err := s.createChainCapability(u)
	if err != nil {
		return err
	}

	capabilityBytes, err := json.Marshal(edvCapability)
	if err != nil {
		return err
	}

	r := &createKeystoreReq{
		Controller: u.controller,
		EDV: &edvOptions{
			// TODO: replace hardcoded URL with the proper s.bddContext.EDVServerURL
			VaultURL:   "https://edv.trustbloc.local:8081" + edvBasePath + "/" + u.vaultID,
			Capability: capabilityBytes,
		},
	}

	return s.createKeystoreReq(u, r, s.bddContext.KeyServerURL+createKeystoreEndpoint)
}

func (s *Steps) createKeystoreReq(u *user, r *createKeystoreReq, endpoint string) error {
	request, err := u.preparePostRequest(r, endpoint)
	if err != nil {
		return err
	}

	request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", u.accessToken))

	response, err := s.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}

	defer func() {
		closeErr := response.Body.Close()
		if closeErr != nil {
			s.logger.Errorf("Failed to close response body: %s\n", closeErr.Error())
		}
	}()

	var resp createKeyStoreResp

	if err := u.processResponse(&resp, response); err != nil {
		return err
	}

	parts := strings.Split(resp.KeyStoreURL, "/")

	u.keystoreID = parts[len(parts)-1]

	if resp.Capability != nil {
		kmsCapability, err := parseRootCapability(resp.Capability)
		if err != nil {
			return fmt.Errorf("parse root capability: %w", err)
		}

		if kmsCapability != nil {
			u.kmsCapability = kmsCapability
		}
	}

	return nil
}

func (s *Steps) createDID(u *user) error {
	uri := buildURI(s.bddContext.KeyServerURL+createDIDEndpoint, u.keystoreID, u.keyID)

	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, uri, nil)
	if err != nil {
		return fmt.Errorf("create DID http request: %w", err)
	}

	request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", u.accessToken))

	response, err := s.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}

	defer func() {
		closeErr := response.Body.Close()
		if closeErr != nil {
			s.logger.Errorf("Failed to close response body: %s\n", closeErr.Error())
		}
	}()

	var resp createDIDResp

	if err := u.processResponse(&resp, response); err != nil {
		return err
	}

	u.edvDID = resp.DID

	return nil
}

func (s *Steps) createChainCapability(u *user) (*zcapld.Capability, error) {
	loader, err := createJSONLDDocumentLoader(mem.NewProvider())
	if err != nil {
		return nil, fmt.Errorf("create document loader: %w", err)
	}

	return zcapld.NewCapability(
		&zcapld.Signer{
			SignatureSuite:     ed25519signature2018.New(suite.WithSigner(u.signer)),
			SuiteType:          ed25519signature2018.SignatureType,
			VerificationMethod: u.controller,
			ProcessorOpts:      []jsonld.ProcessorOpts{jsonld.WithDocumentLoader(loader)},
		},
		zcapld.WithParent(u.edvCapability.ID),
		zcapld.WithInvoker(u.edvDID),
		zcapld.WithAllowedActions("read", "write"),
		zcapld.WithInvocationTarget(u.vaultID, edvResource),
		zcapld.WithCapabilityChain(u.edvCapability.Parent, u.edvCapability.ID))
}

func (s *Steps) makeCreateKeyReq(userName, endpoint, keyType string) error {
	u := s.users[userName]

	req, err := buildCreateKeyReq(u, endpoint, keyType)
	if err != nil {
		return fmt.Errorf("build create key request: %w", err)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}

	defer func() {
		closeErr := resp.Body.Close()
		if closeErr != nil {
			s.logger.Errorf("Failed to close response body: %s\n", closeErr.Error())
		}
	}()

	return processCreateKeyResp(u, resp)
}

func processCreateKeyResp(u *user, resp *http.Response) error {
	var r createKeyResp

	if err := u.processResponse(&r, resp); err != nil {
		return fmt.Errorf("process response: %w", err)
	}

	parts := strings.Split(r.KeyURL, "/")

	u.keyID = parts[len(parts)-1]

	u.data = map[string]string{
		"key_url": r.KeyURL,
	}

	return nil
}

func buildCreateKeyReq(u *user, endpoint, keyType string) (*http.Request, error) {
	r := &createKeyReq{
		KeyType: keyType,
	}

	req, err := u.preparePostRequest(r, endpoint)
	if err != nil {
		return nil, err
	}

	err = u.SetCapabilityInvocation(req, actionCreateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to set capability invocation: %w", err)
	}

	err = u.Sign(req)
	if err != nil {
		return nil, fmt.Errorf("user failed to sign http message: %w", err)
	}

	return req, nil
}

func (s *Steps) makeParallelCreateKeyReqs(userName, endpoint, keyTypes string) error {
	u := s.users[userName]

	var rr []*http.Request

	for _, kt := range strings.Split(keyTypes, ",") {
		r, err := buildCreateKeyReq(u, endpoint, kt)
		if err != nil {
			return fmt.Errorf("build create key request: %w", err)
		}

		rr = append(rr, r)
	}

	statusCh := make(chan string, len(rr))
	errCh := make(chan error)

	for _, r := range rr {
		go func(req *http.Request) {
			usr := &user{name: u.name}

			resp, err := s.httpClient.Do(req)
			if err != nil {
				errCh <- err

				return
			}
			defer resp.Body.Close() //nolint:errcheck // ignore

			err = processCreateKeyResp(usr, resp)
			if err != nil {
				errCh <- err

				return
			}

			statusCh <- usr.response.status
		}(r)
	}

	var multiRespStatus []string

	respCount := len(rr)

	for respCount > 0 {
		select {
		case err := <-errCh:
			return err
		case s := <-statusCh:
			multiRespStatus = append(multiRespStatus, s)
			respCount--
		}
	}

	u.multiRespStatus = multiRespStatus

	return nil
}

func (s *Steps) makeExportPubKeyReq(userName, endpoint string) error {
	u := s.users[userName]

	request, err := u.prepareGetRequest(endpoint)
	if err != nil {
		return err
	}

	err = u.SetCapabilityInvocation(request, actionExportKey)
	if err != nil {
		return fmt.Errorf("user failed to set capability invocation: %w", err)
	}

	err = u.Sign(request)
	if err != nil {
		return fmt.Errorf("user failed to sign request: %w", err)
	}

	response, err := s.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}

	defer func() {
		closeErr := response.Body.Close()
		if closeErr != nil {
			s.logger.Errorf("Failed to close response body: %s\n", closeErr.Error())
		}
	}()

	var exportKeyResponse exportKeyResp

	if respErr := u.processResponse(&exportKeyResponse, response); respErr != nil {
		return respErr
	}

	if exportKeyResponse.KeyType == "" {
		return fmt.Errorf("export public key returned an empty key type")
	}

	u.data = map[string]string{
		"public_key": string(exportKeyResponse.PublicKey),
		"key_type":   string(exportKeyResponse.KeyType),
	}

	return nil
}

func (s *Steps) makeCreateAndExportKeyReq(user, endpoint, keyType string) error {
	u := s.users[user]

	r := &createKeyReq{
		KeyType:   keyType,
		ExportKey: true,
	}

	request, err := u.preparePostRequest(r, endpoint)
	if err != nil {
		return err
	}

	err = u.SetCapabilityInvocation(request, actionCreateKey)
	if err != nil {
		return fmt.Errorf("failed to set capability invocation: %w", err)
	}

	err = u.Sign(request)
	if err != nil {
		return fmt.Errorf("user failed to sign http message: %w", err)
	}

	response, err := s.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}

	defer func() {
		closeErr := response.Body.Close()
		if closeErr != nil {
			s.logger.Errorf("Failed to close response body: %s\n", closeErr.Error())
		}
	}()

	var createKeyResponse createKeyResp

	if respErr := u.processResponse(&createKeyResponse, response); respErr != nil {
		return respErr
	}

	u.data = map[string]string{
		"key_url":    createKeyResponse.KeyURL,
		"public_key": string(createKeyResponse.PublicKey),
	}

	return nil
}

func (s *Steps) makeImportKeyReq(userName, endpoint, keyID string) error {
	u := s.users[userName]

	_, pk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("failed to generate ed25519 key: %w", err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(pk)
	if err != nil {
		return fmt.Errorf("failed to marshal private key: %w", err)
	}

	r := &importKeyReq{
		Key:     der,
		KeyType: "ED25519",
		KeyID:   keyID,
	}

	request, err := u.preparePutRequest(r, endpoint)
	if err != nil {
		return err
	}

	err = u.SetCapabilityInvocation(request, actionImportKey)
	if err != nil {
		return fmt.Errorf("user failed to set capability invocation: %w", err)
	}

	err = u.Sign(request)
	if err != nil {
		return fmt.Errorf("user failed to sign request: %w", err)
	}

	response, err := s.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}

	defer func() {
		closeErr := response.Body.Close()
		if closeErr != nil {
			s.logger.Errorf("Failed to close response body: %s\n", closeErr.Error())
		}
	}()

	var importKeyResponse importKeyResp

	if respErr := u.processResponse(&importKeyResponse, response); respErr != nil {
		return respErr
	}

	u.data = map[string]string{
		"key_url": importKeyResponse.KeyURL,
	}

	return nil
}

func (s *Steps) makeRotateKeyReq(userName, endpoint, keyType string) error {
	u := s.users[userName]

	r := &rotateKeyReq{
		keyType,
	}

	request, err := u.preparePostRequest(r, endpoint)
	if err != nil {
		return err
	}

	err = u.SetCapabilityInvocation(request, actionRotateKey)
	if err != nil {
		return fmt.Errorf("user failed to set capability invocation: %w", err)
	}

	err = u.Sign(request)
	if err != nil {
		return fmt.Errorf("user failed to sign request: %w", err)
	}

	response, err := s.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}

	defer func() {
		closeErr := response.Body.Close()
		if closeErr != nil {
			s.logger.Errorf("Failed to close response body: %s\n", closeErr.Error())
		}
	}()

	var rotateKeyResponse rotateKeyResp

	if respErr := u.processResponse(&rotateKeyResponse, response); respErr != nil {
		return respErr
	}

	parts := strings.Split(rotateKeyResponse.KeyURL, "/")
	u.keyID = parts[len(parts)-1]

	u.data["key_url"] = rotateKeyResponse.KeyURL

	return nil
}

func (s *Steps) makeSignMessageReq(userName, endpoint, message string) error { //nolint:dupl // ignore
	u := s.users[userName]

	r := &signReq{
		Message: []byte(message),
	}

	request, err := u.preparePostRequest(r, endpoint)
	if err != nil {
		return err
	}

	err = u.SetCapabilityInvocation(request, actionSign)
	if err != nil {
		return fmt.Errorf("user failed to set zcap on request: %w", err)
	}

	err = u.Sign(request)
	if err != nil {
		return fmt.Errorf("user failed to sign request: %w", err)
	}

	response, err := s.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}

	defer func() {
		closeErr := response.Body.Close()
		if closeErr != nil {
			s.logger.Errorf("Failed to close response body: %s\n", closeErr.Error())
		}
	}()

	var signResponse signResp

	if respErr := u.processResponse(&signResponse, response); respErr != nil {
		return respErr
	}

	u.data = map[string]string{
		"signature": string(signResponse.Signature),
	}

	return nil
}

func (s *Steps) makeVerifySignatureReq(userName, endpoint, tag, message string) error {
	u := s.users[userName]

	r := &verifyReq{
		Signature: []byte(u.data[tag]),
		Message:   []byte(message),
	}

	return s.makeVerifyReq(u, actionVerify, r, endpoint)
}

func (s *Steps) makeEncryptMessageReq(userName, endpoint, message string) error {
	u := s.users[userName]

	r := &encryptReq{
		Message: []byte(message),
	}

	request, err := u.preparePostRequest(r, endpoint)
	if err != nil {
		return err
	}

	err = u.SetCapabilityInvocation(request, actionEncrypt)
	if err != nil {
		return fmt.Errorf("user failed to set zcap on request: %w", err)
	}

	err = u.Sign(request)
	if err != nil {
		return fmt.Errorf("user failed to sign request: %w", err)
	}

	response, err := s.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}

	defer func() {
		closeErr := response.Body.Close()
		if closeErr != nil {
			s.logger.Errorf("Failed to close response body: %s\n", closeErr.Error())
		}
	}()

	var encryptResponse encryptResp

	if respErr := u.processResponse(&encryptResponse, response); respErr != nil {
		return respErr
	}

	u.data = map[string]string{
		"ciphertext": string(encryptResponse.Ciphertext),
		"nonce":      string(encryptResponse.Nonce),
	}

	return nil
}

func (s *Steps) makeDecryptCipherReq(userName, endpoint, tag string) error {
	u := s.users[userName]

	r := &decryptReq{
		Ciphertext: []byte(u.data[tag]),
		Nonce:      []byte(u.data["nonce"]),
	}

	request, err := u.preparePostRequest(r, endpoint)
	if err != nil {
		return err
	}

	err = u.SetCapabilityInvocation(request, actionDecrypt)
	if err != nil {
		return fmt.Errorf("user failed to set zcap: %w", err)
	}

	err = u.Sign(request)
	if err != nil {
		return fmt.Errorf("user failed to sign request: %w", err)
	}

	response, err := s.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}

	defer func() {
		closeErr := response.Body.Close()
		if closeErr != nil {
			s.logger.Errorf("Failed to close response body: %s\n", closeErr.Error())
		}
	}()

	var decryptResponse decryptResp

	if respErr := u.processResponse(&decryptResponse, response); respErr != nil {
		return respErr
	}

	u.data = map[string]string{
		"plaintext": string(decryptResponse.Plaintext),
	}

	return nil
}

func (s *Steps) makeComputeMACReq(userName, endpoint, data string) error { //nolint:dupl // ignore
	u := s.users[userName]

	r := &computeMACReq{
		Data: []byte(data),
	}

	request, err := u.preparePostRequest(r, endpoint)
	if err != nil {
		return err
	}

	err = u.SetCapabilityInvocation(request, actionComputeMac)
	if err != nil {
		return fmt.Errorf("user failed to set zcap: %w", err)
	}

	err = u.Sign(request)
	if err != nil {
		return fmt.Errorf("user failed to sign request: %w", err)
	}

	response, err := s.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}

	defer func() {
		closeErr := response.Body.Close()
		if closeErr != nil {
			s.logger.Errorf("Failed to close response body: %s\n", closeErr.Error())
		}
	}()

	var computeMACResponse computeMACResp

	if respErr := u.processResponse(&computeMACResponse, response); respErr != nil {
		return respErr
	}

	u.data = map[string]string{
		"mac": string(computeMACResponse.MAC),
	}

	return nil
}

func (s *Steps) makeVerifyMACReq(userName, endpoint, tag, data string) error {
	u := s.users[userName]

	r := &verifyMACReq{
		MAC:  []byte(u.data[tag]),
		Data: []byte(data),
	}

	return s.makeVerifyReq(u, actionVerifyMAC, r, endpoint)
}

func (s *Steps) makeVerifyReq(u *user, action string, r interface{}, endpoint string) error {
	request, err := u.preparePostRequest(r, endpoint)
	if err != nil {
		return err
	}

	err = u.SetCapabilityInvocation(request, action)
	if err != nil {
		return fmt.Errorf("user failed to set zcap on request: %w", err)
	}

	err = u.Sign(request)
	if err != nil {
		return fmt.Errorf("user failed to sign request: %w", err)
	}

	response, err := s.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}

	defer func() {
		closeErr := response.Body.Close()
		if closeErr != nil {
			s.logger.Errorf("Failed to close response body: %s\n", closeErr.Error())
		}
	}()

	return u.processResponse(nil, response)
}

func (s *Steps) makeWrapKeyReq(userName, endpoint, keyID, recipient string) error {
	u := s.users[userName]

	recipientPubKey := u.recipientPubKeys[recipient].parsedKey

	r := &wrapReq{
		CEK: s.keys[keyID],
		APU: []byte("sender"),
		APV: []byte("recipient"),
		RecipientPubKey: &crypto.PublicKey{
			KID:   recipientPubKey.KID,
			X:     recipientPubKey.X,
			Y:     recipientPubKey.Y,
			Curve: recipientPubKey.Curve,
			Type:  recipientPubKey.Type,
		},
	}

	request, err := u.preparePostRequest(r, endpoint)
	if err != nil {
		return err
	}

	err = u.SetCapabilityInvocation(request, actionWrap)
	if err != nil {
		return fmt.Errorf("user failed to set zcap: %w", err)
	}

	err = u.Sign(request)
	if err != nil {
		return fmt.Errorf("user failed to sign request: %w", err)
	}

	response, err := s.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}

	defer func() {
		closeErr := response.Body.Close()
		if closeErr != nil {
			s.logger.Errorf("Failed to close response body: %s\n", closeErr.Error())
		}
	}()

	var wrapResponse wrapResp

	if respErr := u.processResponse(&wrapResponse, response); respErr != nil {
		return respErr
	}

	wrappedKey, err := json.Marshal(wrapResponse.RecipientWrappedKey)
	if err != nil {
		return err
	}

	u.data = map[string]string{
		"wrapped_key": string(wrappedKey),
	}

	return nil
}

func (s *Steps) makeUnwrapKeyReq(userName, endpoint, tag, sender string) error {
	u := s.users[userName]

	wrappedKeyContent := s.users[sender].data[tag]

	var wrappedKey crypto.RecipientWrappedKey

	err := json.Unmarshal([]byte(wrappedKeyContent), &wrappedKey)
	if err != nil {
		return err
	}

	r := &unwrapReq{
		WrappedKey: wrappedKey,
	}

	request, err := u.preparePostRequest(r, endpoint)
	if err != nil {
		return err
	}

	err = u.SetCapabilityInvocation(request, actionUnwrap)
	if err != nil {
		return fmt.Errorf("user failed to set zcap: %w", err)
	}

	err = u.Sign(request)
	if err != nil {
		return fmt.Errorf("user failed to sign request: %w", err)
	}

	response, err := s.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}

	defer func() {
		closeErr := response.Body.Close()
		if closeErr != nil {
			s.logger.Errorf("Failed to close response body: %s\n", closeErr.Error())
		}
	}()

	var unwrapResponse unwrapResp

	if respErr := u.processResponse(&unwrapResponse, response); respErr != nil {
		return respErr
	}

	u.data = map[string]string{
		"key": string(unwrapResponse.Key),
	}

	return nil
}

func (s *Steps) getPubKeyOfRecipient(userName, recipientName string) error {
	u := s.users[userName]

	recipient, ok := s.users[recipientName]
	if !ok {
		return fmt.Errorf("no recipient with name %s exist", recipientName)
	}

	request, err := recipient.prepareGetRequest(s.bddContext.KeyServerURL + exportKeyEndpoint)
	if err != nil {
		return err
	}

	// recipient delegates authority on the user to export their public key
	c, err := delegateCapability(recipient.kmsCapability, recipient.signer, recipient.controller, u.controller)
	if err != nil {
		return fmt.Errorf("delegate capability: %w", err)
	}

	err = setCapabilityHeader(request, base64.URLEncoding.EncodeToString(c), u.controller, u.authKMS, u.authCrypto)
	if err != nil {
		return err
	}

	response, err := s.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}

	defer func() {
		closeErr := response.Body.Close()
		if closeErr != nil {
			s.logger.Errorf("Failed to close response body: %s\n", closeErr.Error())
		}
	}()

	var exportKeyResponse exportKeyResp

	if respErr := recipient.processResponse(&exportKeyResponse, response); respErr != nil {
		return fmt.Errorf("response error: %w", respErr)
	}

	keyData := &publicKeyData{
		rawBytes: exportKeyResponse.PublicKey,
	}

	if key, ok := parsePublicKey(exportKeyResponse.PublicKey); ok {
		keyData.parsedKey = key
	}

	u.recipientPubKeys = map[string]*publicKeyData{
		recipientName: keyData,
	}

	return nil
}

func parsePublicKey(rawBytes []byte) (*crypto.PublicKey, bool) {
	// depending on key type, raw bytes might not represent publicKey structure
	var k crypto.PublicKey
	if err := json.Unmarshal(rawBytes, &k); err != nil {
		return nil, false
	}

	return &k, true
}

func delegateCapability(c *zcapld.Capability, s signer, verificationMethod, invoker string) ([]byte, error) {
	var chain []interface{}

	untyped, ok := c.Proof[0]["capabilityChain"].([]interface{})
	if ok {
		chain = append(chain, untyped...)
	}

	chain = append(chain, c.ID)

	loader, err := createJSONLDDocumentLoader(mem.NewProvider())
	if err != nil {
		return nil, fmt.Errorf("create document loader: %w", err)
	}

	delegatedCapability, err := zcapld.NewCapability(
		&zcapld.Signer{
			SignatureSuite:     ed25519signature2018.New(suite.WithSigner(s)),
			SuiteType:          ed25519signature2018.SignatureType,
			VerificationMethod: verificationMethod,
			ProcessorOpts:      []jsonld.ProcessorOpts{jsonld.WithDocumentLoader(loader)},
		},
		zcapld.WithInvoker(invoker),
		zcapld.WithParent(c.ID),
		zcapld.WithInvocationTarget(c.InvocationTarget.ID, c.InvocationTarget.Type),
		zcapld.WithAllowedActions(actionExportKey),
		zcapld.WithCapabilityChain(chain...),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to delegate zcap unto user: %w", err)
	}

	compressed, err := zcapsvc.CompressZCAP(delegatedCapability)
	if err != nil {
		return nil, fmt.Errorf("failed to compress zcap: %w", err)
	}

	return compressed, nil
}

func setCapabilityHeader(request *http.Request, capability string, controller string,
	k kms.KeyManager, c crypto.Crypto) error {
	request.Header.Set(
		zcapld.CapabilityInvocationHTTPHeader,
		fmt.Sprintf(`zcap capability="%s",action="%s"`, capability, actionExportKey),
	)

	hs := httpsignatures.NewHTTPSignatures(&zcapld.AriesDIDKeySecrets{})
	hs.SetSignatureHashAlgorithm(&zcapld.AriesDIDKeySignatureHashAlgorithm{
		Crypto: c,
		KMS:    k,
	})

	err := hs.Sign(controller, request)
	if err != nil {
		return fmt.Errorf("user failed to sign request: %w", err)
	}

	return nil
}

func (s *Steps) checkRespStatus(user, status string) error {
	u := s.users[user]

	if u.response.status != status {
		return fmt.Errorf("expected HTTP response status %q, got: %q", status, u.response.status)
	}

	return nil
}

func (s *Steps) checkMultiRespStatus(user, status string) error {
	u := s.users[user]

	for _, s := range u.multiRespStatus {
		if s != status {
			return fmt.Errorf("expected HTTP response status %q, got: %q", status, s)
		}
	}

	return nil
}

func (s *Steps) checkHeaderWithValidURL(user, header string) error {
	u := s.users[user]

	_, err := url.ParseRequestURI(u.response.headers[header])
	if err != nil {
		return fmt.Errorf("expected %q header to be a valid URL, got error: %w", header, err)
	}

	return nil
}

func (s *Steps) checkRespWithNonEmptyValue(user, tag string) error {
	u := s.users[user]

	if u.data[tag] == "" {
		return fmt.Errorf("expected property %q to be non-empty", tag)
	}

	return nil
}

func (s *Steps) checkRespWithNoValue(user, tag string) error {
	u := s.users[user]

	v, ok := u.data[tag]
	if ok {
		return fmt.Errorf("expected no field %q, got with value: %q", tag, v)
	}

	return nil
}

func (s *Steps) checkRespWithValue(user, tag, val string) error {
	u := s.users[user]

	expected := regexp.MustCompile(val)

	if !expected.MatchString(u.data[tag]) {
		return fmt.Errorf("expected %q to match %q, got: %q", tag, val, u.data[tag])
	}

	return nil
}

func (s *Steps) checkRespWithKeyContent(user, keyID string) error {
	u := s.users[user]

	key := []byte(u.data["key"])

	if !bytes.Equal(key, s.keys[keyID]) {
		return fmt.Errorf("expected key content to be %q, got: %q", base64.URLEncoding.EncodeToString(s.keys[keyID]),
			base64.URLEncoding.EncodeToString(key))
	}

	return nil
}

type ldStoreProvider struct {
	ContextStore        ldstore.ContextStore
	RemoteProviderStore ldstore.RemoteProviderStore
}

func (p *ldStoreProvider) JSONLDContextStore() ldstore.ContextStore {
	return p.ContextStore
}

func (p *ldStoreProvider) JSONLDRemoteProviderStore() ldstore.RemoteProviderStore {
	return p.RemoteProviderStore
}

func createJSONLDDocumentLoader(storageProvider storage.Provider) (*ld.DocumentLoader, error) {
	contextStore, err := ldstore.NewContextStore(storageProvider)
	if err != nil {
		return nil, fmt.Errorf("create JSON-LD context store: %w", err)
	}

	remoteProviderStore, err := ldstore.NewRemoteProviderStore(storageProvider)
	if err != nil {
		return nil, fmt.Errorf("create remote provider store: %w", err)
	}

	ldStore := &ldStoreProvider{
		ContextStore:        contextStore,
		RemoteProviderStore: remoteProviderStore,
	}

	documentLoader, err := ld.NewDocumentLoader(ldStore)
	if err != nil {
		return nil, fmt.Errorf("new document loader: %w", err)
	}

	return documentLoader, nil
}

package openshift

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/containers/image/docker"
	"github.com/containers/image/manifest"
	"github.com/containers/image/types"
	"github.com/containers/image/version"
)

// openshiftClient is configuration for dealing with a single image stream, for reading or writing.
type openshiftClient struct {
	ref     openshiftReference
	baseURL *url.URL
	// Values from Kubernetes configuration
	httpClient  *http.Client
	bearerToken string // "" if not used
	username    string // "" if not used
	password    string // if username != ""
}

// newOpenshiftClient creates a new openshiftClient for the specified reference.
func newOpenshiftClient(ref openshiftReference) (*openshiftClient, error) {
	// We have already done this parsing in ParseReference, but thrown away
	// httpClient. So, parse again.
	// (We could also rework/split restClientFor to "get base URL" to be done
	// in ParseReference, and "get httpClient" to be done here.  But until/unless
	// we support non-default clusters, this is good enough.)

	// Overall, this is modelled on openshift/origin/pkg/cmd/util/clientcmd.New().ClientConfig() and openshift/origin/pkg/client.
	cmdConfig := defaultClientConfig()
	logrus.Debugf("cmdConfig: %#v", cmdConfig)
	restConfig, err := cmdConfig.ClientConfig()
	if err != nil {
		return nil, err
	}
	// REMOVED: SetOpenShiftDefaults (values are not overridable in config files, so hard-coded these defaults.)
	logrus.Debugf("restConfig: %#v", restConfig)
	baseURL, httpClient, err := restClientFor(restConfig)
	if err != nil {
		return nil, err
	}
	logrus.Debugf("URL: %#v", *baseURL)

	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	return &openshiftClient{
		ref:         ref,
		baseURL:     baseURL,
		httpClient:  httpClient,
		bearerToken: restConfig.BearerToken,
		username:    restConfig.Username,
		password:    restConfig.Password,
	}, nil
}

// doRequest performs a correctly authenticated request to a specified path, and returns response body or an error object.
func (c *openshiftClient) doRequest(method, path string, requestBody []byte) ([]byte, error) {
	url := *c.baseURL
	url.Path = path
	var requestBodyReader io.Reader
	if requestBody != nil {
		logrus.Debugf("Will send body: %s", requestBody)
		requestBodyReader = bytes.NewReader(requestBody)
	}
	req, err := http.NewRequest(method, url.String(), requestBodyReader)
	if err != nil {
		return nil, err
	}

	if len(c.bearerToken) != 0 {
		req.Header.Set("Authorization", "Bearer "+c.bearerToken)
	} else if len(c.username) != 0 {
		req.SetBasicAuth(c.username, c.password)
	}
	req.Header.Set("Accept", "application/json, */*")
	req.Header.Set("User-Agent", fmt.Sprintf("skopeo/%s", version.Version))
	if requestBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	logrus.Debugf("%s %s", method, url)
	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	logrus.Debugf("Got body: %s", body)
	// FIXME: Just throwing this useful information away only to try to guess later...
	logrus.Debugf("Got content-type: %s", res.Header.Get("Content-Type"))

	var status status
	statusValid := false
	if err := json.Unmarshal(body, &status); err == nil && len(status.Status) > 0 {
		statusValid = true
	}

	switch {
	case res.StatusCode == http.StatusSwitchingProtocols: // FIXME?! No idea why this weird case exists in k8s.io/kubernetes/pkg/client/restclient.
		if statusValid && status.Status != "Success" {
			return nil, errors.New(status.Message)
		}
	case res.StatusCode >= http.StatusOK && res.StatusCode <= http.StatusPartialContent:
		// OK.
	default:
		if statusValid {
			return nil, errors.New(status.Message)
		}
		return nil, fmt.Errorf("HTTP error: status code: %d, body: %s", res.StatusCode, string(body))
	}

	return body, nil
}

// getImage loads the specified image object.
func (c *openshiftClient) getImage(imageStreamImageName string) (*image, error) {
	// FIXME: validate components per validation.IsValidPathSegmentName?
	path := fmt.Sprintf("/oapi/v1/namespaces/%s/imagestreamimages/%s@%s", c.ref.namespace, c.ref.stream, imageStreamImageName)
	body, err := c.doRequest("GET", path, nil)
	if err != nil {
		return nil, err
	}
	// Note: This does absolutely no kind/version checking or conversions.
	var isi imageStreamImage
	if err := json.Unmarshal(body, &isi); err != nil {
		return nil, err
	}
	return &isi.Image, nil
}

// convertDockerImageReference takes an image API DockerImageReference value and returns a reference we can actually use;
// currently OpenShift stores the cluster-internal service IPs here, which are unusable from the outside.
func (c *openshiftClient) convertDockerImageReference(ref string) (string, error) {
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("Invalid format of docker reference %s: missing '/'", ref)
	}
	return c.ref.dockerReference.Hostname() + "/" + parts[1], nil
}

type openshiftImageSource struct {
	client *openshiftClient
	// Values specific to this image
	ctx                        *types.SystemContext
	requestedManifestMIMETypes []string
	// State
	docker               types.ImageSource // The Docker Registry endpoint, or nil if not resolved yet
	imageStreamImageName string            // Resolved image identifier, or "" if not known yet
}

// newImageSource creates a new ImageSource for the specified reference,
// asking the backend to use a manifest from requestedManifestMIMETypes if possible.
// nil requestedManifestMIMETypes means manifest.DefaultRequestedManifestMIMETypes.
// The caller must call .Close() on the returned ImageSource.
func newImageSource(ctx *types.SystemContext, ref openshiftReference, requestedManifestMIMETypes []string) (types.ImageSource, error) {
	client, err := newOpenshiftClient(ref)
	if err != nil {
		return nil, err
	}

	return &openshiftImageSource{
		client: client,
		ctx:    ctx,
		requestedManifestMIMETypes: requestedManifestMIMETypes,
	}, nil
}

// Reference returns the reference used to set up this source, _as specified by the user_
// (not as the image itself, or its underlying storage, claims).  This can be used e.g. to determine which public keys are trusted for this image.
func (s *openshiftImageSource) Reference() types.ImageReference {
	return s.client.ref
}

// Close removes resources associated with an initialized ImageSource, if any.
func (s *openshiftImageSource) Close() {
	if s.docker != nil {
		s.docker.Close()
		s.docker = nil
	}
}

func (s *openshiftImageSource) GetTargetManifest(digest string) ([]byte, string, error) {
	if err := s.ensureImageIsResolved(); err != nil {
		return nil, "", err
	}
	return s.docker.GetTargetManifest(digest)
}

func (s *openshiftImageSource) GetManifest() ([]byte, string, error) {
	if err := s.ensureImageIsResolved(); err != nil {
		return nil, "", err
	}
	return s.docker.GetManifest()
}

// GetBlob returns a stream for the specified blob, and the blob’s size (or -1 if unknown).
func (s *openshiftImageSource) GetBlob(digest string) (io.ReadCloser, int64, error) {
	if err := s.ensureImageIsResolved(); err != nil {
		return nil, 0, err
	}
	return s.docker.GetBlob(digest)
}

func (s *openshiftImageSource) GetSignatures() ([][]byte, error) {
	if err := s.ensureImageIsResolved(); err != nil {
		return nil, err
	}

	image, err := s.client.getImage(s.imageStreamImageName)
	if err != nil {
		return nil, err
	}
	var sigs [][]byte
	for _, sig := range image.Signatures {
		if sig.Type == imageSignatureTypeAtomic {
			sigs = append(sigs, sig.Content)
		}
	}
	return sigs, nil
}

// ensureImageIsResolved sets up s.docker and s.imageStreamImageName
func (s *openshiftImageSource) ensureImageIsResolved() error {
	if s.docker != nil {
		return nil
	}

	// FIXME: validate components per validation.IsValidPathSegmentName?
	path := fmt.Sprintf("/oapi/v1/namespaces/%s/imagestreams/%s", s.client.ref.namespace, s.client.ref.stream)
	body, err := s.client.doRequest("GET", path, nil)
	if err != nil {
		return err
	}
	// Note: This does absolutely no kind/version checking or conversions.
	var is imageStream
	if err := json.Unmarshal(body, &is); err != nil {
		return err
	}
	var te *tagEvent
	for _, tag := range is.Status.Tags {
		if tag.Tag != s.client.ref.dockerReference.Tag() {
			continue
		}
		if len(tag.Items) > 0 {
			te = &tag.Items[0]
			break
		}
	}
	if te == nil {
		return fmt.Errorf("No matching tag found")
	}
	logrus.Debugf("tag event %#v", te)
	dockerRefString, err := s.client.convertDockerImageReference(te.DockerImageReference)
	if err != nil {
		return err
	}
	logrus.Debugf("Resolved reference %#v", dockerRefString)
	dockerRef, err := docker.ParseReference("//" + dockerRefString)
	if err != nil {
		return err
	}
	d, err := dockerRef.NewImageSource(s.ctx, s.requestedManifestMIMETypes)
	if err != nil {
		return err
	}
	s.docker = d
	s.imageStreamImageName = te.Image
	return nil
}

type openshiftImageDestination struct {
	client *openshiftClient
	docker types.ImageDestination // The Docker Registry endpoint
	// State
	imageStreamImageName string // "" if not yet known
}

// newImageDestination creates a new ImageDestination for the specified reference.
func newImageDestination(ctx *types.SystemContext, ref openshiftReference) (types.ImageDestination, error) {
	client, err := newOpenshiftClient(ref)
	if err != nil {
		return nil, err
	}

	// FIXME: Should this always use a digest, not a tag? Uploading to Docker by tag requires the tag _inside_ the manifest to match,
	// i.e. a single signed image cannot be available under multiple tags.  But with types.ImageDestination, we don't know
	// the manifest digest at this point.
	dockerRefString := fmt.Sprintf("//%s/%s/%s:%s", client.ref.dockerReference.Hostname(), client.ref.namespace, client.ref.stream, client.ref.dockerReference.Tag())
	dockerRef, err := docker.ParseReference(dockerRefString)
	if err != nil {
		return nil, err
	}
	docker, err := dockerRef.NewImageDestination(ctx)
	if err != nil {
		return nil, err
	}

	return &openshiftImageDestination{
		client: client,
		docker: docker,
	}, nil
}

// Reference returns the reference used to set up this destination.  Note that this should directly correspond to user's intent,
// e.g. it should use the public hostname instead of the result of resolving CNAMEs or following redirects.
func (d *openshiftImageDestination) Reference() types.ImageReference {
	return d.client.ref
}

// Close removes resources associated with an initialized ImageDestination, if any.
func (d *openshiftImageDestination) Close() {
	d.docker.Close()
}

func (d *openshiftImageDestination) SupportedManifestMIMETypes() []string {
	return []string{
		manifest.DockerV2Schema1SignedMediaType,
		manifest.DockerV2Schema1MediaType,
	}
}

// SupportsSignatures returns an error (to be displayed to the user) if the destination certainly can't store signatures.
// Note: It is still possible for PutSignatures to fail if SupportsSignatures returns nil.
func (d *openshiftImageDestination) SupportsSignatures() error {
	return nil
}

// ShouldCompressLayers returns true iff it is desirable to compress layer blobs written to this destination.
func (d *openshiftImageDestination) ShouldCompressLayers() bool {
	return true
}

// PutBlob writes contents of stream and returns data representing the result (with all data filled in).
// inputInfo.Digest can be optionally provided if known; it is not mandatory for the implementation to verify it.
// inputInfo.Size is the expected length of stream, if known.
// WARNING: The contents of stream are being verified on the fly.  Until stream.Read() returns io.EOF, the contents of the data SHOULD NOT be available
// to any other readers for download using the supplied digest.
// If stream.Read() at any time, ESPECIALLY at end of input, returns an error, PutBlob MUST 1) fail, and 2) delete any data stored so far.
func (d *openshiftImageDestination) PutBlob(stream io.Reader, inputInfo types.BlobInfo) (types.BlobInfo, error) {
	return d.docker.PutBlob(stream, inputInfo)
}

func (d *openshiftImageDestination) PutManifest(m []byte) error {
	manifestDigest, err := manifest.Digest(m)
	if err != nil {
		return err
	}
	d.imageStreamImageName = manifestDigest

	return d.docker.PutManifest(m)
}

func (d *openshiftImageDestination) PutSignatures(signatures [][]byte) error {
	if d.imageStreamImageName == "" {
		return fmt.Errorf("Internal error: Unknown manifest digest, can't add signatures")
	}
	// Because image signatures are a shared resource in Atomic Registry, the default upload
	// always adds signatures.  Eventually we should also allow removing signatures.

	if len(signatures) == 0 {
		return nil // No need to even read the old state.
	}

	image, err := d.client.getImage(d.imageStreamImageName)
	if err != nil {
		return err
	}
	existingSigNames := map[string]struct{}{}
	for _, sig := range image.Signatures {
		existingSigNames[sig.objectMeta.Name] = struct{}{}
	}

sigExists:
	for _, newSig := range signatures {
		for _, existingSig := range image.Signatures {
			if existingSig.Type == imageSignatureTypeAtomic && bytes.Equal(existingSig.Content, newSig) {
				continue sigExists
			}
		}

		// The API expect us to invent a new unique name. This is racy, but hopefully good enough.
		var signatureName string
		for {
			randBytes := make([]byte, 16)
			n, err := rand.Read(randBytes)
			if err != nil || n != 16 {
				return fmt.Errorf("Error generating random signature ID: %v, len %d", err, n)
			}
			signatureName = fmt.Sprintf("%s@%032x", d.imageStreamImageName, randBytes)
			if _, ok := existingSigNames[signatureName]; !ok {
				break
			}
		}
		// Note: This does absolutely no kind/version checking or conversions.
		sig := imageSignature{
			typeMeta: typeMeta{
				Kind:       "ImageSignature",
				APIVersion: "v1",
			},
			objectMeta: objectMeta{Name: signatureName},
			Type:       imageSignatureTypeAtomic,
			Content:    newSig,
		}
		body, err := json.Marshal(sig)
		_, err = d.client.doRequest("POST", "/oapi/v1/imagesignatures", body)
		if err != nil {
			return err
		}
	}

	return nil
}

// Commit marks the process of storing the image as successful and asks for the image to be persisted.
// WARNING: This does not have any transactional semantics:
// - Uploaded data MAY be visible to others before Commit() is called
// - Uploaded data MAY be removed or MAY remain around if Close() is called without Commit() (i.e. rollback is allowed but not guaranteed)
func (d *openshiftImageDestination) Commit() error {
	return d.docker.Commit()
}

// These structs are subsets of github.com/openshift/origin/pkg/image/api/v1 and its dependencies.
type imageStream struct {
	Status imageStreamStatus `json:"status,omitempty"`
}
type imageStreamStatus struct {
	DockerImageRepository string              `json:"dockerImageRepository"`
	Tags                  []namedTagEventList `json:"tags,omitempty"`
}
type namedTagEventList struct {
	Tag   string     `json:"tag"`
	Items []tagEvent `json:"items"`
}
type tagEvent struct {
	DockerImageReference string `json:"dockerImageReference"`
	Image                string `json:"image"`
}
type imageStreamImage struct {
	Image image `json:"image"`
}
type image struct {
	objectMeta           `json:"metadata,omitempty"`
	DockerImageReference string `json:"dockerImageReference,omitempty"`
	//	DockerImageMetadata        runtime.RawExtension `json:"dockerImageMetadata,omitempty"`
	DockerImageMetadataVersion string `json:"dockerImageMetadataVersion,omitempty"`
	DockerImageManifest        string `json:"dockerImageManifest,omitempty"`
	//	DockerImageLayers          []ImageLayer         `json:"dockerImageLayers"`
	Signatures []imageSignature `json:"signatures,omitempty"`
}

const imageSignatureTypeAtomic string = "atomic"

type imageSignature struct {
	typeMeta   `json:",inline"`
	objectMeta `json:"metadata,omitempty"`
	Type       string `json:"type"`
	Content    []byte `json:"content"`
	// Conditions []SignatureCondition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	// ImageIdentity string `json:"imageIdentity,omitempty"`
	// SignedClaims map[string]string `json:"signedClaims,omitempty"`
	// Created *unversioned.Time `json:"created,omitempty"`
	// IssuedBy SignatureIssuer `json:"issuedBy,omitempty"`
	// IssuedTo SignatureSubject `json:"issuedTo,omitempty"`
}
type typeMeta struct {
	Kind       string `json:"kind,omitempty"`
	APIVersion string `json:"apiVersion,omitempty"`
}
type objectMeta struct {
	Name                       string            `json:"name,omitempty"`
	GenerateName               string            `json:"generateName,omitempty"`
	Namespace                  string            `json:"namespace,omitempty"`
	SelfLink                   string            `json:"selfLink,omitempty"`
	ResourceVersion            string            `json:"resourceVersion,omitempty"`
	Generation                 int64             `json:"generation,omitempty"`
	DeletionGracePeriodSeconds *int64            `json:"deletionGracePeriodSeconds,omitempty"`
	Labels                     map[string]string `json:"labels,omitempty"`
	Annotations                map[string]string `json:"annotations,omitempty"`
}

// A subset of k8s.io/kubernetes/pkg/api/unversioned/Status
type status struct {
	Status  string `json:"status,omitempty"`
	Message string `json:"message,omitempty"`
	// Reason StatusReason `json:"reason,omitempty"`
	// Details *StatusDetails `json:"details,omitempty"`
	Code int32 `json:"code,omitempty"`
}

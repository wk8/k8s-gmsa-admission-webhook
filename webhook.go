package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"

	"github.com/sirupsen/logrus"
	admissionv1beta1 "k8s.io/api/admission/v1beta1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/pkg/serviceaccount"
)

const (
	// these 3 constants are the coordinates of the Custom Resource Definition
	crdAPIGroup     = "windows.k8s.io"
	crdAPIVersion   = "v1alpha1"
	crdResourceName = "gmsacredentialspecs"

	// crdContentsField is the single field that's expect to be defined in a GMSA CRD,
	// and to contain the contents of the cred spec itself
	crdContentsField = "credspec"

	// gMSAContainerSpecContentsAnnotationKeySuffix is the suffix of the pod annotation where we store
	// the contents of the GMSA credential spec for a given container (the full annotation being
	// the container's name with this suffix appended).
	gMSAContainerSpecContentsAnnotationKeySuffix = ".container.alpha.windows.kubernetes.io/gmsa-credential-spec"
	// gMSAPodSpecContentsAnnotationKey is the pod annotation where we store the contents of the GMSA
	// credential spec to use for containers that do not have their own specific GMSA cred spec set via a
	// gMSAContainerSpecContentsAnnotationKeySuffix annotation as explained above
	gMSAPodSpecContentsAnnotationKey = "pod.alpha.windows.kubernetes.io/gmsa-credential-spec"

	// gMSAContainerSpecNameAnnotationKeySuffix is the suffix of the pod annotation used
	// to give the name of the GMSA credential spec for a given container (the full annotation
	// being the container's name with this suffix appended).
	gMSAContainerSpecNameAnnotationKeySuffix = gMSAContainerSpecContentsAnnotationKeySuffix + "-name"
	// gMSAPodSpecNameAnnotationKey is the pod annotation used to give the name of the GMSA
	// credential spec for containers that do not have their own specific GMSA cred spec name
	// set via a gMSAContainerSpecNameAnnotationKeySuffix annotation as explained above
	gMSAPodSpecNameAnnotationKey = gMSAPodSpecContentsAnnotationKey + "-name"

	// notFound is used in `isNotFoundError` below
	notFound = "not found"
)

type webhook struct {
	server        *http.Server
	coreClient    kubernetes.Interface
	dynamicClient dynamic.Interface
}

type podAdmissionError struct {
	error
	code int
	pod  *corev1.Pod
}

func newWebhook(coreClient kubernetes.Interface, dynamicClient dynamic.Interface) *webhook {
	return &webhook{
		coreClient:    coreClient,
		dynamicClient: dynamicClient,
	}
}

// start is a blocking call.
func (webhook *webhook) start(port int, tlsConfig *tlsConfig) error {
	if webhook.server != nil {
		return fmt.Errorf("webhook already started")
	}

	webhook.server = &http.Server{
		Addr:    ":" + strconv.Itoa(port),
		Handler: webhook,
	}

	logrus.Debugf("starting webhook server at port %v", port)
	var err error
	if tlsConfig == nil {
		err = webhook.server.ListenAndServe()
	} else {
		err = webhook.server.ListenAndServeTLS(tlsConfig.crtPath, tlsConfig.keyPath)
	}

	if err != nil {
		if err == http.ErrServerClosed {
			logrus.Debugf("server closed")
		} else {
			return err
		}
	}

	return nil
}

// stop stops the HTTP server
func (webhook *webhook) stop() error {
	if webhook.server == nil {
		return fmt.Errorf("webhook server not started yet")
	}
	return webhook.server.Shutdown(context.Background())
}

// ServeHTTP makes this object a http.Handler
func (webhook *webhook) ServeHTTP(responseWriter http.ResponseWriter, request *http.Request) {
	// only one endpoint, no need for a router here
	if request.URL.Path != "/validate-mutate" {
		logrus.Infof("received request for unknown path %s", request.URL.Path)
		responseWriter.WriteHeader(http.StatusNotFound)
		return
	}

	admissionResponse := webhook.httpRequestToAdmissionResponse(request)

	responseAdmissionReview := admissionv1beta1.AdmissionReview{Response: admissionResponse}
	if responseBytes, err := json.Marshal(responseAdmissionReview); err == nil {
		logrus.Debugf("sending response: %s", responseBytes)

		if _, err = responseWriter.Write(responseBytes); err != nil {
			logrus.Errorf("error when writing response JSON %s: %v", responseBytes, err)
		}
	} else {
		logrus.Errorf("error when marshalling response %v: %v", responseAdmissionReview, err)
	}
}

// httpRequestToAdmissionResponse turns a raw HTTP request into an AdmissionResponse struct.
func (webhook *webhook) httpRequestToAdmissionResponse(request *http.Request) *admissionv1beta1.AdmissionResponse {
	// verify the content type is accurate
	contentType := request.Header.Get("Content-Type")
	if contentType != "application/json" {
		return deniedAdmissionResponse(fmt.Errorf("expected JSON content-type header"), http.StatusUnsupportedMediaType)
	}

	// read the body
	if request.Body == nil {
		deniedAdmissionResponse(fmt.Errorf("no request body"), http.StatusBadRequest)
	}
	body, err := ioutil.ReadAll(request.Body)
	if err != nil {
		return deniedAdmissionResponse(fmt.Errorf("couldn't read request body: %v", err), http.StatusBadRequest)
	}

	logrus.Debugf("handling request: %s", body)

	// unmarshall the request
	admissionReview := admissionv1beta1.AdmissionReview{}
	if err = json.Unmarshal(body, &admissionReview); err != nil {
		return deniedAdmissionResponse(fmt.Errorf("unable to unmarshall JSON body as an admission review: %v", err), http.StatusBadRequest)
	}
	if admissionReview.Request == nil {
		return deniedAdmissionResponse(fmt.Errorf("no 'Request' field in JSON body"), http.StatusBadRequest)
	}

	admissionResponse, admissionError := webhook.validateAndMutate(admissionReview.Request)
	if admissionError != nil {
		admissionResponse = deniedAdmissionResponse(admissionError)
	}

	// return the same UID
	admissionResponse.UID = admissionReview.Request.UID

	return admissionResponse
}

// validateAndMutate is where the non-HTTP-related work happens.
func (webhook *webhook) validateAndMutate(request *admissionv1beta1.AdmissionRequest) (*admissionv1beta1.AdmissionResponse, *podAdmissionError) {
	if request.Kind.Kind != "Pod" {
		return nil, &podAdmissionError{error: fmt.Errorf("expected a pod object, got a %v", request.Kind.Kind), code: http.StatusBadRequest}
	}

	pod, err := unmarshallPod(request.Object)
	if err != nil {
		return nil, err
	}

	switch request.Operation {
	case admissionv1beta1.Create:
		return webhook.validateAndMutateCreateRequest(pod)
	case admissionv1beta1.Update:
		oldPod, err := unmarshallPod(request.OldObject)
		if err != nil {
			return nil, err
		}
		return validateUpdateRequest(pod, oldPod)
	default:
		return nil, &podAdmissionError{error: fmt.Errorf("unpexpected operation %s", request.Operation), pod: pod, code: http.StatusBadRequest}
	}
}

// unmarshallPod unmarshalls a pod object from its raw JSON representation.
func unmarshallPod(object runtime.RawExtension) (*corev1.Pod, *podAdmissionError) {
	pod := &corev1.Pod{}
	if err := json.Unmarshal(object.Raw, pod); err != nil {
		return nil, &podAdmissionError{error: fmt.Errorf("unable to unmarshall pod JSON object: %v", err), code: http.StatusBadRequest}
	}

	return pod, nil
}

// validateAndMutateCreateRequest makes sure that pods using GMSA's are created using ServiceAccounts
// which are indeed authorized to use the requested GMSA's, and inlines them into the pod's spec as annotations.
func (webhook *webhook) validateAndMutateCreateRequest(pod *corev1.Pod) (*admissionv1beta1.AdmissionResponse, *podAdmissionError) {
	var patches []map[string]string

	patches, err := webhook.validateAndInlineSingleGMSASpec(pod, gMSAPodSpecNameAnnotationKey, gMSAPodSpecContentsAnnotationKey, patches)
	if err != nil {
		return nil, err
	}

	for _, container := range pod.Spec.Containers {
		patches, err = webhook.validateAndInlineSingleGMSASpec(
			pod,
			container.Name+gMSAContainerSpecNameAnnotationKeySuffix,
			container.Name+gMSAContainerSpecContentsAnnotationKeySuffix,
			patches,
		)
		if err != nil {
			return nil, err
		}
	}

	admissionResponse := &admissionv1beta1.AdmissionResponse{Allowed: true}

	if len(patches) != 0 {
		patchesBytes, e := json.Marshal(patches)
		if e != nil {
			return nil, &podAdmissionError{error: fmt.Errorf("unable to marshall patch JSON %v: %v", patches, err), pod: pod, code: http.StatusInternalServerError}
		}

		admissionResponse.Patch = patchesBytes
		patchType := admissionv1beta1.PatchTypeJSONPatch
		admissionResponse.PatchType = &patchType
	}

	return admissionResponse, nil
}

// validateAndInlineSingleGMSASpec inlines the contents of the GMSA spec named by the nameKey annotation
// into the contentsKey annotation, provided that it exists and that the service account associated to
// the pod can `use` that GMSA spec.
func (webhook *webhook) validateAndInlineSingleGMSASpec(pod *corev1.Pod, nameKey, contentsKey string, patches []map[string]string) ([]map[string]string, *podAdmissionError) {
	// only this admission controller is allowed to populate the actual contents of the cred spec
	if _, present := pod.Annotations[contentsKey]; present {
		return patches, &podAdmissionError{error: fmt.Errorf("cannot pre-set a pod's gMSA content annotation (annotation %v present)", contentsKey), pod: pod, code: http.StatusForbidden}
	}

	credSpecName, present := pod.Annotations[nameKey]
	if !present || credSpecName == "" {
		// nothing to do
		return patches, nil
	}

	// let's check that the associated service account can read the relevant cred spec CRD
	if authorized, reason := webhook.isAuthorizedToReadCredSpec(pod, credSpecName); !authorized {
		msg := fmt.Sprintf("the service account used for this pod does not have `use` access to the %s gMSA cred spec", credSpecName)
		if reason != "" {
			msg += fmt.Sprintf(", reason : %s", reason)
		}
		return patches, &podAdmissionError{error: fmt.Errorf(msg), pod: pod, code: http.StatusForbidden}
	}

	// finally inline the config map's contents into the spec
	resource := schema.GroupVersionResource{
		Group:    crdAPIGroup,
		Version:  crdAPIVersion,
		Resource: crdResourceName,
	}
	credSpec, err := webhook.dynamicClient.Resource(resource).Get(credSpecName, metav1.GetOptions{})
	if err != nil {
		admissionError := &podAdmissionError{pod: pod}

		if isNotFoundError(err) {
			admissionError.error = fmt.Errorf("cred spec %s does not exist", credSpecName)
			admissionError.code = http.StatusNotFound
		} else {
			admissionError.error = fmt.Errorf("unable to retrieve the contents of cred spec %s: %v", credSpecName, err)
			admissionError.code = http.StatusInternalServerError
		}

		return patches, admissionError
	}

	if contents, present := credSpec.Object[crdContentsField]; !present || contents == "" {
		return patches, &podAdmissionError{error: fmt.Errorf("cred spec %s does not have a %s key", credSpecName, crdContentsField), pod: pod, code: http.StatusExpectationFailed}
	}

	contentsBytes, err := json.Marshal(credSpec.Object[crdContentsField])
	if err != nil {
		return patches, &podAdmissionError{error: fmt.Errorf("unable to marshall cred spec %s: %v", credSpecName, err), code: http.StatusInternalServerError}
	}

	patches = append(patches, map[string]string{
		"op":    "add",
		"path":  fmt.Sprintf("/metadata/annotations/%s", contentsKey),
		"value": string(contentsBytes),
	})

	return patches, nil
}

// isAuthorizedToReadConfigMap checks that the pod's service account is authorized to `use` that cred spec.
func (webhook *webhook) isAuthorizedToReadCredSpec(pod *corev1.Pod, credSpecName string) (bool, string) {
	servceAccountUserInfo := serviceaccount.UserInfo(pod.Namespace, pod.Spec.ServiceAccountName, "")

	// needed to cast `authorizationv1.ExtraValue` to `[]string`
	var extra map[string]authorizationv1.ExtraValue
	for k, v := range servceAccountUserInfo.GetExtra() {
		extra[k] = v
	}

	subjectAccessReview := authorizationv1.LocalSubjectAccessReview{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: pod.Namespace,
		},
		Spec: authorizationv1.SubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: pod.Namespace,
				Verb:      "use",
				Group:     crdAPIGroup,
				Version:   crdAPIVersion,
				Resource:  crdResourceName,
				Name:      credSpecName,
			},
			User:   servceAccountUserInfo.GetName(),
			Groups: servceAccountUserInfo.GetGroups(),
			UID:    servceAccountUserInfo.GetUID(),
			Extra:  extra,
		},
	}

	response, err := webhook.coreClient.AuthorizationV1().LocalSubjectAccessReviews(pod.Namespace).Create(&subjectAccessReview)
	if err != nil {
		return false, fmt.Sprintf("could not check authz access: %v", err.Error())
	}
	return response.Status.Allowed && !response.Status.Denied, response.Status.Reason
}

// isNotFoundError returns true if the error indicates "not found".  It parses
// the error string looking for known values, which is imperfect but works in
// practice; and there's not much better we can do right now with k8s' dynamic client API
func isNotFoundError(err error) bool {
	msg := err.Error()
	return msg[len(msg)-len(notFound):] == notFound
}

// validateUpdateRequest ensures that there are no updates to any of the GMSA annotations.
func validateUpdateRequest(pod, oldPod *corev1.Pod) (*admissionv1beta1.AdmissionResponse, *podAdmissionError) {
	errors := make([]*podAdmissionError, 0)
	errors = append(errors,
		assertAnnotationsUnchanged(pod, oldPod, gMSAPodSpecNameAnnotationKey),
		assertAnnotationsUnchanged(pod, oldPod, gMSAPodSpecContentsAnnotationKey))

	for _, container := range pod.Spec.Containers {
		errors = append(errors,
			assertAnnotationsUnchanged(pod, oldPod, container.Name+gMSAContainerSpecNameAnnotationKeySuffix),
			assertAnnotationsUnchanged(pod, oldPod, container.Name+gMSAContainerSpecContentsAnnotationKeySuffix))
	}

	for _, err := range errors {
		if err != nil {
			return nil, err
		}
	}

	return &admissionv1beta1.AdmissionResponse{Allowed: true}, nil
}

// assertAnnotationsUnchanged returns an error if the two pods don't have the same annotation for the given key.
func assertAnnotationsUnchanged(pod, oldPod *corev1.Pod, key string) *podAdmissionError {
	if pod.Annotations[key] != oldPod.Annotations[key] {
		return &podAdmissionError{
			error: fmt.Errorf("cannot update an existing pod's gMSA annotation (annotation %v changed)", key),
			pod:   pod,
			code:  http.StatusForbidden,
		}
	}
	return nil
}

// deniedAdmissionResponse is a helper function to create an AdmissionResponse
// with an embedded error.
func deniedAdmissionResponse(err error, httpCode ...int) *admissionv1beta1.AdmissionResponse {
	var code int
	logMsg := "refusing to admit"

	if admissionError, ok := err.(podAdmissionError); ok {
		code = admissionError.code
		if admissionError.pod != nil {
			logMsg += fmt.Sprintf(" pod %+v", admissionError.pod)
		}
	}

	if len(httpCode) > 0 {
		code = httpCode[0]
	}

	if code != 0 {
		logMsg += fmt.Sprintf(" with code %v", code)
	}

	logrus.Infof(logMsg+": %v", err.Error())

	return &admissionv1beta1.AdmissionResponse{
		Allowed: false,
		Result: &metav1.Status{
			Message: err.Error(),
			Code:    int32(code),
		},
	}
}

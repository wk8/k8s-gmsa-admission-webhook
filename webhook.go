package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"
	admissionv1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
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
)

// jsonPatchEscapeReplacer complies with JSON Patch's way of escaping special characters
// in key names. See https://tools.ietf.org/html/rfc6901#section-3
var jsonPatchEscaper = strings.NewReplacer("~", "~0", "/", "~1")

type webhook struct {
	server *http.Server
	client kubeClientInterface
}

type webhookOperation string

const (
	validate webhookOperation = "VALIDATE"
	mutate   webhookOperation = "MUTATE"
)

type podAdmissionError struct {
	error
	code int
	pod  *corev1.Pod
}

func newWebhook(client kubeClientInterface) *webhook {
	return &webhook{client: client}
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

	logrus.Infof("starting webhook server at port %v", port)
	var err error
	if tlsConfig == nil {
		err = webhook.server.ListenAndServe()
	} else {
		err = webhook.server.ListenAndServeTLS(tlsConfig.crtPath, tlsConfig.keyPath)
	}

	if err != nil {
		if err == http.ErrServerClosed {
			logrus.Infof("server closed")
		} else {
			return err
		}
	}

	return nil
}

// stop stops the HTTP server.
func (webhook *webhook) stop() error {
	if webhook.server == nil {
		return fmt.Errorf("webhook server not started yet")
	}
	return webhook.server.Shutdown(context.Background())
}

// ServeHTTP makes this object a http.Handler.
// Since we only have a couple of endpoints, there's no need for a full-fleged router here.
func (webhook *webhook) ServeHTTP(responseWriter http.ResponseWriter, request *http.Request) {
	var admissionResponse *admissionv1beta1.AdmissionResponse

	switch request.URL.Path {
	case "/validate":
		admissionResponse = webhook.httpRequestToAdmissionResponse(request, validate)
	case "/mutate":
		admissionResponse = webhook.httpRequestToAdmissionResponse(request, mutate)
	default:
		logrus.Infof("received POST request for unknown path %s", request.URL.Path)
		responseWriter.WriteHeader(http.StatusNotFound)
		return
	}

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
func (webhook *webhook) httpRequestToAdmissionResponse(request *http.Request, operation webhookOperation) *admissionv1beta1.AdmissionResponse {
	// should be a POST request
	if strings.ToUpper(request.Method) != "POST" {
		return deniedAdmissionResponse(fmt.Errorf("expected POST HTTP request"), http.StatusMethodNotAllowed)
	}
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

	logrus.Debugf("handling %s request: %s", operation, body)

	// unmarshall the request
	admissionReview := admissionv1beta1.AdmissionReview{}
	if err = json.Unmarshal(body, &admissionReview); err != nil {
		return deniedAdmissionResponse(fmt.Errorf("unable to unmarshall JSON body as an admission review: %v", err), http.StatusBadRequest)
	}
	if admissionReview.Request == nil {
		return deniedAdmissionResponse(fmt.Errorf("no 'Request' field in JSON body"), http.StatusBadRequest)
	}

	admissionResponse, admissionError := webhook.validateOrMutate(admissionReview.Request, operation)
	if admissionError != nil {
		admissionResponse = deniedAdmissionResponse(admissionError)
	}

	// return the same UID
	admissionResponse.UID = admissionReview.Request.UID

	return admissionResponse
}

// validateOrMutate is where the non-HTTP-related work happens.
func (webhook *webhook) validateOrMutate(request *admissionv1beta1.AdmissionRequest, operation webhookOperation) (*admissionv1beta1.AdmissionResponse, *podAdmissionError) {
	if request.Kind.Kind != "Pod" {
		return nil, &podAdmissionError{error: fmt.Errorf("expected a pod object, got a %v", request.Kind.Kind), code: http.StatusBadRequest}
	}

	pod, err := unmarshallPod(request.Object)
	if err != nil {
		return nil, err
	}

	switch request.Operation {
	case admissionv1beta1.Create:
		switch operation {
		case validate:
			return webhook.validateCreateRequest(pod, request.Namespace)
		case mutate:
			return webhook.mutateCreateRequest(pod)
		default:
			// shouldn't happen, but needed so that all paths in the function have a return value
			panic(fmt.Errorf("unexpected webhook operation: %v", operation))
		}

	case admissionv1beta1.Update:
		if operation == validate {
			oldPod, err := unmarshallPod(request.OldObject)
			if err != nil {
				return nil, err
			}
			return validateUpdateRequest(pod, oldPod)
		}

		// we only do validation on updates, no mutation
		return &admissionv1beta1.AdmissionResponse{Allowed: true}, nil
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

// validateCreateRequest ensures that the only GMSA content annotations set on the pod,
// match the corresponding GMSA name annotations, and that the pod's service account
// is authorized to `use` the requested GMSA's.
func (webhook *webhook) validateCreateRequest(pod *corev1.Pod, namespace string) (*admissionv1beta1.AdmissionResponse, *podAdmissionError) {
	var err *podAdmissionError

	iterateOverGMSAAnnotationPairs(pod, func(nameKey, contentsKey string) {
		if err != nil {
			return
		}

		if credSpecName, present := pod.Annotations[nameKey]; present && credSpecName != "" {
			// let's check that the associated service account can read the relevant cred spec CRD
			if authorized, reason := webhook.client.isAuthorizedToUseCredSpec(pod.Spec.ServiceAccountName, namespace, credSpecName); !authorized {
				msg := fmt.Sprintf("service account %s does not have `use` access to the %s gMSA cred spec", pod.Spec.ServiceAccountName, credSpecName)
				if reason != "" {
					msg += fmt.Sprintf(", reason : %s", reason)
				}
				err = &podAdmissionError{error: fmt.Errorf(msg), pod: pod, code: http.StatusForbidden}
				return
			}

			// and the content annotation should contain the expected cred spec
			if credSpecContents, present := pod.Annotations[contentsKey]; present {
				if expectedContents, code, retrieveErr := webhook.client.retrieveCredSpecContents(credSpecName); retrieveErr != nil {
					err = &podAdmissionError{error: retrieveErr, pod: pod, code: code}
				} else if credSpecContents != expectedContents {
					err = &podAdmissionError{error: fmt.Errorf("cred spec contained in annotation %s does not match the contents of GMSA %s", contentsKey, credSpecName), pod: pod, code: http.StatusForbidden}
				}
			}

		} else if _, present := pod.Annotations[contentsKey]; present {
			// the name annotation is not present, but the content one is
			err = &podAdmissionError{error: fmt.Errorf("cannot pre-set a pod's gMSA content annotation (annotation %v present)", contentsKey), pod: pod, code: http.StatusForbidden}
		}
	})
	if err != nil {
		return nil, err
	}

	return &admissionv1beta1.AdmissionResponse{Allowed: true}, nil
}

// mutateCreateRequest inlines the requested GMSA's into the pod's spec as annotations.
func (webhook *webhook) mutateCreateRequest(pod *corev1.Pod) (*admissionv1beta1.AdmissionResponse, *podAdmissionError) {
	var (
		patches []map[string]string
		err     *podAdmissionError
	)

	iterateOverGMSAAnnotationPairs(pod, func(nameKey, contentsKey string) {
		if err != nil {
			return
		}

		if _, present := pod.Annotations[contentsKey]; present {
			// only this admission controller is allowed to populate the actual contents of the cred spec
			// and "/mutate" is called before "/validate"
			err = &podAdmissionError{error: fmt.Errorf("cannot pre-set a pod's gMSA content annotation (annotation %v present)", contentsKey), pod: pod, code: http.StatusForbidden}
		} else if credSpecName, present := pod.Annotations[nameKey]; present && credSpecName != "" {
			if contents, code, retrieveErr := webhook.client.retrieveCredSpecContents(credSpecName); retrieveErr != nil {
				err = &podAdmissionError{error: retrieveErr, pod: pod, code: code}
			} else {
				// worth noting that this JSON patch is guaranteed to work since we know at this point
				// that the pod has annotations, and and that it doesn't have this specific one
				patches = append(patches, map[string]string{
					"op":    "add",
					"path":  fmt.Sprintf("/metadata/annotations/%s", jsonPatchEscaper.Replace(contentsKey)),
					"value": contents,
				})
			}
		}
	})
	if err != nil {
		return nil, err
	}

	admissionResponse := &admissionv1beta1.AdmissionResponse{Allowed: true}

	if len(patches) != 0 {
		patchesBytes, err := json.Marshal(patches)
		if err != nil {
			return nil, &podAdmissionError{error: fmt.Errorf("unable to marshall patch JSON %v: %v", patches, err), pod: pod, code: http.StatusInternalServerError}
		}

		admissionResponse.Patch = patchesBytes
		patchType := admissionv1beta1.PatchTypeJSONPatch
		admissionResponse.PatchType = &patchType
	}

	return admissionResponse, nil
}

// validateUpdateRequest ensures that there are no updates to any of the GMSA annotations.
func validateUpdateRequest(pod, oldPod *corev1.Pod) (*admissionv1beta1.AdmissionResponse, *podAdmissionError) {
	var err *podAdmissionError

	iterateOverGMSAAnnotationPairs(pod, func(nameKey, contentsKey string) {
		if err != nil {
			return
		}
		if nameKeyErr := assertAnnotationsUnchanged(pod, oldPod, nameKey); nameKeyErr != nil {
			err = nameKeyErr
		} else if contentsKeyErr := assertAnnotationsUnchanged(pod, oldPod, contentsKey); contentsKeyErr != nil {
			err = contentsKeyErr
		}
	})
	if err != nil {
		return nil, err
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

// iterateOverGMSAAnnotationPairs calls `f` on the successive pairs of GMSA name and contents
// annotation keys.
func iterateOverGMSAAnnotationPairs(pod *corev1.Pod, f func(nameKey, contentsKey string)) {
	f(gMSAPodSpecNameAnnotationKey, gMSAPodSpecContentsAnnotationKey)
	for _, container := range pod.Spec.Containers {
		f(container.Name+gMSAContainerSpecNameAnnotationKeySuffix, container.Name+gMSAContainerSpecContentsAnnotationKeySuffix)
	}
}

// deniedAdmissionResponse is a helper function to create an AdmissionResponse
// with an embedded error.
func deniedAdmissionResponse(err error, httpCode ...int) *admissionv1beta1.AdmissionResponse {
	var code int
	logMsg := "refusing to admit"

	if admissionError, ok := err.(*podAdmissionError); ok {
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

	logrus.Infof("%s: %v", logMsg, err)

	return &admissionv1beta1.AdmissionResponse{
		Allowed: false,
		Result: &metav1.Status{
			Message: err.Error(),
			Code:    int32(code),
		},
	}
}

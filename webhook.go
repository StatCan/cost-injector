package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/ghodss/yaml"
	"github.com/golang/glog"
	"k8s.io/api/admission/v1beta1"
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

// Based on https://github.com/hmcts/k8s-env-injector

var (
	runtimeScheme = runtime.NewScheme()
	codecs        = serializer.NewCodecFactory(runtimeScheme)
	deserializer  = codecs.UniversalDeserializer()
)

var ignoredNamespaces = []string{
	metav1.NamespaceSystem,
	metav1.NamespacePublic,
}

const (
	admissionWebhookInject = "cost-injector-webhook-inject"
	admissionWebhookStatus = "cost-injector-webhook-status"

	WorkloadsAll   = "_all"
	EnvVarNotFound = -1
)

type MutatingWebhookServer struct {
	config *Config
	server *http.Server
}

type MutatingWebhookServerParams struct {
	configFile string
}

type Config struct {
	Annotations map[string]map[string]map[string]string `yaml:"annotations"`
	Env         map[string]map[string][]corev1.EnvVar   `yaml:"env"`
}

type patchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

func init() {
	_ = corev1.AddToScheme(runtimeScheme)
	_ = admissionregistrationv1beta1.AddToScheme(runtimeScheme)
}

func indexOfEnvVar(needle corev1.EnvVar, haystack []corev1.EnvVar) int {
	for indx, envvar := range haystack {
		if envvar.Name == needle.Name {
			return indx
		}
	}

	return EnvVarNotFound
}

func createPatch(pod *corev1.Pod, config *Config, annotations map[string]string) ([]byte, error) {
	var patches []patchOperation

	if namespaceEnv, ok := config.Env[pod.Namespace]; ok {
		envvars := make([]corev1.EnvVar, 0)

		// Add environment variables from WorkloadsAll
		if allEnvVars, ok := namespaceEnv[WorkloadsAll]; ok {
			for _, envvar := range allEnvVars {
				envvars = append(envvars, envvar)
			}
		}

		// Add environment variables for app.kubernetes.io/name label value on the pod
		if name, ok := pod.ObjectMeta.Labels["app.kubernetes.io/name"]; ok {
			if nameEnvVars, ok := namespaceEnv[name]; ok {
				for _, envvar := range nameEnvVars {
					// Check if we already have this env var from WorkloadsAll,
					// and if we do, replace it rather than add it.
					if indx := indexOfEnvVar(envvar, envvars); indx >= 0 {
						envvars[indx] = envvar
					} else {
						envvars = append(envvars, envvar)
					}
				}
			}
		}

		for index, container := range pod.Spec.Containers {
			patches = append(patches, updateEnv(container.Env, envvars, fmt.Sprintf("/spec/containers/%d/env", index))...)
		}
	}

	if namespaceAnnotations, ok := config.Annotations[pod.Namespace]; ok {
		// Add annotations from WorkloadsAll
		if allAnnotations, ok := namespaceAnnotations[WorkloadsAll]; ok {
			for k, v := range allAnnotations {
				annotations[k] = v
			}
		}

		// Add environment variables for app.kubernetes.io/name label value on the pod
		if name, ok := pod.ObjectMeta.Labels["app.kubernetes.io/name"]; ok {
			if nameAnnotations, ok := namespaceAnnotations[name]; ok {
				for k, v := range nameAnnotations {
					annotations[k] = v
				}
			}
		}
	}

	patches = append(patches, updateAnnotation(pod.Annotations, annotations)...)

	return json.Marshal(patches)
}

func loadConfig(configFile string) (*Config, error) {
	data, err := ioutil.ReadFile(configFile)
	if err != nil {
		return nil, err
	}
	glog.Infof("Reading the new configuration: sha256sum %x", sha256.Sum256(data))

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	glog.Infof("Loaded the new configuration: %+v", &cfg)

	return &cfg, nil
}

func mutateReq(ignoredList []string, metadata *metav1.ObjectMeta) bool {
	for _, namespace := range ignoredList {
		if metadata.Namespace == namespace {
			glog.Infof("Skipping the mutation for %v in namespace: %v", metadata.Name, metadata.Namespace)
			return false
		}
	}

	annotations := metadata.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	status := annotations[admissionWebhookStatus]

	var required bool
	if strings.ToLower(status) == "injected" {
		required = false
	} else {
		bval, err := strconv.ParseBool(annotations[admissionWebhookInject])
		if err != nil {
			required = true
		} else {
			required = bval
		}
	}

	glog.Infof("Mutation for %v/%v: status: %q required:%v", metadata.Namespace, metadata.Name, status, required)
	return required
}

func updateEnv(target, envVars []corev1.EnvVar, basePath string) (patch []patchOperation) {
	first := len(target) == 0
	var value interface{}
	for _, envVar := range envVars {
		value = envVar
		path := basePath
		if first {
			first = false
			value = []corev1.EnvVar{envVar}
		} else {
			path = path + "/-"
		}
		patch = append(patch, patchOperation{
			Op:    "add",
			Path:  path,
			Value: value,
		})
	}
	return patch
}

func updateAnnotation(target map[string]string, annotations map[string]string) (patch []patchOperation) {
	for k, v := range annotations {
		if target == nil {
			target = map[string]string{}
			patch = append(patch, patchOperation{
				Op:   "add",
				Path: "/metadata/annotations",
				Value: map[string]string{
					k: v,
				},
			})
		} else if target[k] == "" {
			target = map[string]string{}
			patch = append(patch, patchOperation{
				Op:    "add",
				Path:  "/metadata/annotations/" + k,
				Value: v,
			})
		} else {
			patch = append(patch, patchOperation{
				Op:    "replace",
				Path:  "/metadata/annotations/" + k,
				Value: v,
			})
		}
	}
	return patch
}

func (mutatingwebhookserver *MutatingWebhookServer) mutateHandle(w http.ResponseWriter, r *http.Request) {
	body, err := ioutil.ReadAll(r.Body)
	defer r.Body.Close()
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "%s", err)
		return
	}

	admissionReview := v1beta1.AdmissionReview{}
	if err := json.Unmarshal(body, &admissionReview); err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "%s", err)
		return
	}

	response, err := mutatingwebhookserver.mutate(admissionReview.Request)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "%s", err)
		return
	}

	reviewResponse := v1beta1.AdmissionReview{
		Response: response,
	}

	if body, err = json.Marshal(reviewResponse); err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "%s", err)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

func (mutatingwebhookserver *MutatingWebhookServer) mutate(request *v1beta1.AdmissionRequest) (*v1beta1.AdmissionResponse, error) {
	var err error
	pod := v1.Pod{}

	if err := json.Unmarshal(request.Object.Raw, &pod); err != nil {
		glog.Errorf("Unable to unmarshal the raw object: %v", err)
		return &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}, nil
	}

	glog.Infof("AdmissionReview for Kind=%v, Namespace=%v Name=%v (%v) UID=%v patchOperation=%v UserInfo=%v",
		request.Kind, request.Namespace, request.Name, request.Name, request.UID, request.Operation, request.UserInfo)

	if !mutateReq(ignoredNamespaces, &pod.ObjectMeta) {
		glog.Infof("Skipping the mutation for %s/%s", pod.Namespace, pod.Name)
		return &v1beta1.AdmissionResponse{
			Allowed: true,
		}, nil
	}

	annotations := map[string]string{admissionWebhookStatus: "injected"}
	patchBytes, err := createPatch(&pod, mutatingwebhookserver.config, annotations)
	if err != nil {
		return &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}, nil
	}

	glog.Infof("AdmissionResponse: patch=%v\n", string(patchBytes))
	return &v1beta1.AdmissionResponse{
		Allowed: true,
		Patch:   patchBytes,
		PatchType: func() *v1beta1.PatchType {
			pt := v1beta1.PatchTypeJSONPatch
			return &pt
		}(),
	}, nil
}

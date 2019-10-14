package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"reflect"
	"strconv"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/operator-framework/operator-sdk/pkg/k8sutil"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	"github.com/keycloak/keycloak-operator/pkg/model"
	routev1 "github.com/openshift/api/route/v1"
	"k8s.io/api/admission/v1beta1"
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
)

const (
	webhookName = "operator-webhook"
	servicePort = 8787
)

var scheme = runtime.NewScheme()
var codecs = serializer.NewCodecFactory(scheme)

func init() {
	addToScheme(scheme)
}

func addToScheme(scheme *runtime.Scheme) {
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(v1beta1.AddToScheme(scheme))
	utilruntime.Must(admissionregistrationv1beta1.AddToScheme(scheme))
}

// toAdmissionResponse is a helper function to create an AdmissionResponse
// with an embedded error
func toAdmissionResponse(err error) *v1beta1.AdmissionResponse {
	return &v1beta1.AdmissionResponse{
		Result: &metav1.Status{
			Message: err.Error(),
		},
	}
}

// admitFunc is the type we use for all of our validators and mutators
type admitFunc func(v1beta1.AdmissionReview) *v1beta1.AdmissionResponse

func strPtr(s string) *string { return &s }

func serveWebHookEndpoints() {
	log.Info("In ServeWebHookEndpoints")
	// var config Config
	// config.addFlags()
	// flag.Parse()

	http.HandleFunc("/validate-deployment", serveAlwaysDeny)

	server := &http.Server{
		Addr: ":" + strconv.FormatInt(int64(servicePort), 10),
		// TLSConfig: configTLS(config),
	}
	go func() {
		if err := server.ListenAndServe(); err != nil {
			log.Error(err, "unable to start serveWebHook server")
		}
	}()

	// server.ListenAndServeTLS("", "")
}

func serveAlwaysDeny(w http.ResponseWriter, r *http.Request) {
	serve(w, r, alwaysDeny)
}

// alwaysDeny all requests made to this function.
func alwaysDeny(ar v1beta1.AdmissionReview) *v1beta1.AdmissionResponse {
	// log.Info("calling validate-deployment")
	log.Info(fmt.Sprintf("USERINFO: %+v\n", ar.Request.UserInfo))
	// log.Info(fmt.Sprintf("Kind: %+v\n", ar.Request.Kind))
	// log.Info(fmt.Sprintf("Resource: %+v\n", ar.Request.Resource))
	// log.Info(fmt.Sprintf("SubResource: %+v\n", ar.Request.SubResource))
	// log.Info(fmt.Sprintf("Name: %+v\n", ar.Request.Name))
	// log.Info(fmt.Sprintf("Namespace: %+v\n", ar.Request.Namespace))
	// log.Info(fmt.Sprintf("Operation: %+v\n", ar.Request.Operation))

	// obj := struct {
	// 	metav1.ObjectMeta
	// 	Data map[string]string
	// }{}
	reviewResponse := v1beta1.AdmissionResponse{}
	oldDeployment := &appsv1.Deployment{}
	newDeployment := &appsv1.Deployment{}
	ownerReferences := []metav1.OwnerReference{}

	// Get new deployment
	newObjectRaw := ar.Request.Object.Raw
	if len(newObjectRaw) != 0 {
		// log.Info("newObject present")
		// log.Info(fmt.Sprintf("newObjectRaw: %+v\n", newObjectRaw))
		err := json.Unmarshal(newObjectRaw, &newDeployment)
		if err != nil {
			log.Error(err, "Unable to unmarshal old object")
			return toAdmissionResponse(err)
		}
		// log.Info(fmt.Sprintf("newDeployment: %+v\n", newDeployment))
		ownerReferences = newDeployment.GetOwnerReferences()
	}

	// Get old deployment
	oldObjectRaw := ar.Request.OldObject.Raw
	if len(oldObjectRaw) != 0 {
		// log.Info("oldObject present")
		// log.Info(fmt.Sprintf("oldObjectRaw: %+v\n", oldObjectRaw))
		err := json.Unmarshal(oldObjectRaw, &oldDeployment)
		if err != nil {
			log.Error(err, "Unable to unmarshal old object")
			return toAdmissionResponse(err)
		}
		// log.Info(fmt.Sprintf("oldDeployment: %+v\n", oldDeployment))
		ownerReferences = oldDeployment.GetOwnerReferences()
	}

	// log.Info(fmt.Sprintf("ownerReferences: %+v\n", ownerReferences))

	// Is valid if it is not a secondary resource of the CRs
	if len(ownerReferences) == 0 || (ownerReferences[0].Kind != "Keycloak" && ownerReferences[0].Kind != "KeycloakRealm") {
		log.Info(fmt.Sprintf("Not owned by CRs. Deployment Name: %v", newDeployment.Name))
		reviewResponse.Allowed = true
		return &reviewResponse
	}

	// Is not valid if the spec has been changed
	if !reflect.DeepEqual(oldDeployment.Spec, oldDeployment.Spec) {
		log.Info("Not allowed to modify this CRs deployment spec")
		reviewResponse.Allowed = false
		reviewResponse.Result = &metav1.Status{Message: "Not allowed to modify this CRs deployment spec"}
		return &reviewResponse
	}

	// Check if existing labels have been changed
	labelsMatch := true
	for k, v := range model.PostgresqlDeploymentLabels {
		if newDeployment.ObjectMeta.Labels[k] != v {
			labelsMatch = false
		}
	}
	if !labelsMatch {
		log.Info("Not allowed to change existing labels on CR deployment")
		reviewResponse.Allowed = false
		reviewResponse.Result = &metav1.Status{Message: "Not allowed to change existing labels on CR deployment"}
		return &reviewResponse
	}

	log.Info("Valid change")
	reviewResponse.Allowed = true
	return &reviewResponse
}

func getAdmissionOwnerReference(oldObject metav1.Object, newObject metav1.Object) {

}

/*
 *
 * WEBHOOK RESOURCE CREATION
 *
 */

func createWebHookResources(ctx context.Context, cfg *rest.Config) error {
	namespace, err := k8sutil.GetOperatorNamespace()
	if err != nil {
		log.Error(err, "Failed to get operator namespace")
		return err
	}

	client, err := crclient.New(cfg, crclient.Options{})
	if err != nil {
		return err
	}

	ownRef, err := getPodOwnerRef(ctx, client, namespace)
	if err != nil {
		log.Error(err, "Failed to get pod owner reference")
		return err
	}

	if err = createService(ctx, client, namespace, *ownRef); err != nil {
		return err
	}

	if err = createRoute(ctx, client, namespace, *ownRef); err != nil {
		return err
	}

	webhookUrl, err := getWebhookRouteUrl(ctx, cfg, namespace)
	if err != nil {
		return err
	}

	if err = createValidatingWebhook(ctx, client, namespace, webhookUrl, *ownRef); err != nil {
		return err
	}

	return nil
}

func createService(ctx context.Context, client crclient.Client, namespace string, ownRef metav1.OwnerReference) error {
	log.Info("In createService")
	operatorName, err := k8sutil.GetOperatorName()
	if err != nil {
		log.Error(err, "Failed to get operator name")
		return err
	}

	serviceLabels := map[string]string{"name": operatorName}
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      webhookName,
			Labels: map[string]string{
				"app": webhookName,
			},
		},
		Spec: v1.ServiceSpec{
			Selector: serviceLabels,
			Ports: []v1.ServicePort{
				{
					Protocol:   "TCP",
					Port:       443,
					Name:       webhookName,
					TargetPort: intstr.IntOrString{Type: intstr.Int, IntVal: servicePort},
				},
			},
		},
	}

	service.SetOwnerReferences([]metav1.OwnerReference{ownRef})

	if err = client.Create(ctx, service); err != nil {
		log.Error(err, "Failed to create webhook service")
		return err
	}

	return nil
}

func createRoute(ctx context.Context, client crclient.Client, namespace string, ownRef metav1.OwnerReference) error {
	log.Info("In createRoute")

	route := &routev1.Route{
		ObjectMeta: metav1.ObjectMeta{
			Name:      webhookName,
			Namespace: namespace,
			Labels: map[string]string{
				"app": webhookName,
			},
		},
		Spec: routev1.RouteSpec{
			Port: &routev1.RoutePort{
				TargetPort: intstr.FromString(webhookName),
			},
			TLS: &routev1.TLSConfig{
				Termination: "edge",
			},
			To: routev1.RouteTargetReference{
				Kind: "Service",
				Name: webhookName,
			},
		},
	}

	route.SetOwnerReferences([]metav1.OwnerReference{ownRef})

	if err := client.Create(ctx, route); err != nil {
		log.Error(err, "Failed to create webhook route")
		return err
	}

	return nil
}

func getWebhookRouteUrl(ctx context.Context, cfg *rest.Config, namespace string) (string, error) {
	routeSelector := crclient.ObjectKey{
		Name:      webhookName,
		Namespace: namespace,
	}
	webhookRoute := &routev1.Route{}

	client, err := crclient.New(cfg, crclient.Options{})
	if err != nil {
		return "", err
	}

	if err := client.Get(ctx, routeSelector, webhookRoute); err != nil {
		log.Error(err, "Failed to get webhook route url")
		return "", err
	}

	return webhookRoute.Spec.Host, nil
}

func createValidatingWebhook(ctx context.Context, client crclient.Client, namespace string, webhookUrl string, ownRef metav1.OwnerReference) error {
	log.Info("In createValidatingWebhook")
	failurePolicy := admissionregistrationv1beta1.Fail

	webhook := &admissionregistrationv1beta1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace + "-" + webhookName + "-configuration",
			Labels: map[string]string{
				"app": webhookName,
			},
		},
		Webhooks: []admissionregistrationv1beta1.Webhook{
			{
				Name: "deny-webhook-configuration-deletions.k8s.io",
				Rules: []admissionregistrationv1beta1.RuleWithOperations{{
					Operations: []admissionregistrationv1beta1.OperationType{admissionregistrationv1beta1.Create, admissionregistrationv1beta1.Update, admissionregistrationv1beta1.Delete},
					Rule: admissionregistrationv1beta1.Rule{
						APIGroups:   []string{"*"},
						APIVersions: []string{"*"},
						// Resources:   []string{"pods", "secrets", "deployments", "routes", "ingresses", "services", "statefulsets", "servicemonitors", "grafanadashboards", "prometheusrules"},
						Resources: []string{"deployments"},
					},
				}},
				ClientConfig: admissionregistrationv1beta1.WebhookClientConfig{
					URL: strPtr("https://" + webhookUrl + "/validate-deployment"),
				},
				FailurePolicy: &failurePolicy,
			},
		},
	}

	webhook.SetOwnerReferences([]metav1.OwnerReference{ownRef})

	if err := client.Create(ctx, webhook); err != nil {
		log.Error(err, "Failed to create webhook service")
	}
	return nil
}

/*
 *
 * UTILITY FUNCTIONS
 *
 */

// serve handles the http portion of a request prior to handing to an admit
// function
func serve(w http.ResponseWriter, r *http.Request, admit admitFunc) {
	var body []byte
	if r.Body != nil {
		if data, err := ioutil.ReadAll(r.Body); err == nil {
			body = data
		}
	}

	// verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		log.Info("contentType=%s, expect application/json", contentType)
		return
	}

	// log.Info(fmt.Sprintf("handling request: %s", body))

	// The AdmissionReview that was sent to the webhook
	requestedAdmissionReview := v1beta1.AdmissionReview{}

	// The AdmissionReview that will be returned
	responseAdmissionReview := v1beta1.AdmissionReview{}

	deserializer := codecs.UniversalDeserializer()
	if _, _, err := deserializer.Decode(body, nil, &requestedAdmissionReview); err != nil {
		log.Error(err, "Unable to decode request body")
		responseAdmissionReview.Response = toAdmissionResponse(err)
	} else {
		// pass to admitFunc
		responseAdmissionReview.Response = admit(requestedAdmissionReview)
	}

	// Return the same UID
	responseAdmissionReview.Response.UID = requestedAdmissionReview.Request.UID

	// log.Info(fmt.Sprintf("sending response: %v", responseAdmissionReview.Response))

	respBytes, err := json.Marshal(responseAdmissionReview)
	if err != nil {
		log.Error(err, "unable to marshal json")
	}
	if _, err := w.Write(respBytes); err != nil {
		log.Error(err, "unable to write response")
	}
}

func getPodOwnerRef(ctx context.Context, client crclient.Client, ns string) (*metav1.OwnerReference, error) {
	log.Info("In getPodOwnerRef")
	// Get current Pod the operator is running in
	pod, err := k8sutil.GetPod(ctx, client, ns)
	if err != nil {
		return nil, err
	}
	podOwnerRefs := metav1.NewControllerRef(pod, pod.GroupVersionKind())
	// Get Owner that the Pod belongs to
	ownerRef := metav1.GetControllerOf(pod)
	finalOwnerRef, err := findFinalOwnerRef(ctx, client, ns, ownerRef)
	if err != nil {
		return nil, err
	}
	if finalOwnerRef != nil {
		return finalOwnerRef, nil
	}

	// Default to returning Pod as the Owner
	return podOwnerRefs, nil
}

// findFinalOwnerRef tries to locate the final controller/owner based on the owner reference provided.
func findFinalOwnerRef(ctx context.Context, client crclient.Client, ns string, ownerRef *metav1.OwnerReference) (*metav1.OwnerReference, error) {
	log.Info("In findFinalOwnerRef")
	if ownerRef == nil {
		return nil, nil
	}

	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(ownerRef.APIVersion)
	obj.SetKind(ownerRef.Kind)
	err := client.Get(ctx, types.NamespacedName{Namespace: ns, Name: ownerRef.Name}, obj)
	if err != nil {
		return nil, err
	}
	newOwnerRef := metav1.GetControllerOf(obj)
	if newOwnerRef != nil {
		return findFinalOwnerRef(ctx, client, ns, newOwnerRef)
	}

	log.V(1).Info("Pods owner found", "Kind", ownerRef.Kind, "Name", ownerRef.Name, "Namespace", ns)
	return ownerRef, nil
}

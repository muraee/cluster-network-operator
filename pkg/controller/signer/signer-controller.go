package signer

import (
	"context"
	"fmt"
	"log"

	cnoclient "github.com/openshift/cluster-network-operator/pkg/client"
	"github.com/openshift/cluster-network-operator/pkg/controller/statusmanager"
	"github.com/openshift/library-go/pkg/crypto"
	csrv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"

	"k8s.io/client-go/kubernetes"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const signerName = "network.openshift.io/signer"

// Add controller and start it when the Manager is started.
func Add(mgr manager.Manager, status *statusmanager.StatusManager, _ *cnoclient.Client) error {
	reconciler, err := newReconciler(mgr, status)
	if err != nil {
		return err
	}
	return add(mgr, reconciler)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, status *statusmanager.StatusManager) (reconcile.Reconciler, error) {
	// We need a clientset in order to UpdateApproval() of the CertificateSigningRequest
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, err
	}
	return &ReconcileCSR{client: mgr.GetClient(), scheme: mgr.GetScheme(), status: status, clientset: clientset}, nil
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("signer-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to CetificateSigningRequest resource
	err = c.Watch(&source.Kind{Type: &csrv1.CertificateSigningRequest{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcileCSR{}

// ReconcileCSR reconciles a cluster CertificateSigningRequest object. This
// will watch for changes to CertificateSigningRequest resources with
// SignerName == signerName. It will automatically approve these requests for
// signing. This assumes that the cluster has been configured in a way that
// no bad actors can make certificate signing requests. In future, we may decide
// to implement a scheme that would use a one-time token to validate a request.
//
// All requests will be signed using a CA, that is currently generated by
// the OperatorPKI, and the signed certificate will be returned in the status.
//
// This allows clients to get a signed certificate while maintaining
// private key confidentiality.
type ReconcileCSR struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client crclient.Client
	scheme *runtime.Scheme
	status *statusmanager.StatusManager

	// Note: We need a Clientset as the controller-runtime client does not
	// support non-CRUD subresources (see
	// https://github.com/kubernetes-sigs/controller-runtime/issues/452)
	// This may risk invalidating the cache but in our case, this is not a
	// problem as we only use this to update the approval status of the csr.
	clientset *kubernetes.Clientset
}

// Reconcile CSR
func (r *ReconcileCSR) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	csr := &csrv1.CertificateSigningRequest{}
	err := r.client.Get(ctx, request.NamespacedName, csr)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Return and don't requeue as the CSR has been deleted.
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Println(err)

		return reconcile.Result{}, err
	}

	// Only handle CSRs for this signer
	if csr.Spec.SignerName != signerName {
		// Don't handle a CSR for another signerName. We don't need to log this as
		// we will pollute the logs. We also don't need to requeue it.
		return reconcile.Result{}, nil
	}

	if len(csr.Status.Certificate) != 0 {
		// Request already has a certificate. There is nothing
		// to do as we will, currently, not re-certify or handle any updates to
		// CSRs.
		return reconcile.Result{}, nil
	}

	// We will make the assumption that anyone with permission to issue a
	// certificate signing request to this signer is automatically approved. This
	// is somewhat protected by permissions on the CSR resource.
	// TODO: We may need a more robust way to do this later
	if !isCertificateRequestApproved(csr) {
		csr.Status.Conditions = append(csr.Status.Conditions, csrv1.CertificateSigningRequestCondition{
			Type:    csrv1.CertificateApproved,
			Status:  "True",
			Reason:  "AutoApproved",
			Message: "Automatically approved by " + signerName})
		// Update status to "Approved"
		//nolint:staticcheck
		csr, err = r.clientset.CertificatesV1().CertificateSigningRequests().UpdateApproval(ctx, request.Name, csr, metav1.UpdateOptions{})
		if err != nil {
			log.Printf("Unable to approve certificate for %v and signer %v: %v", request.Name, signerName, err)
			return reconcile.Result{}, err
		}

		// As the update from UpdateApproval() will get reconciled, we
		// no longer need to deal with this request
		return reconcile.Result{}, nil
	}

	// From this, point we are dealing with an approved CSR

	// Get our CA that was created by the operatorpki.
	caSecret := &corev1.Secret{}
	err = r.client.Get(ctx, types.NamespacedName{Namespace: "openshift-ovn-kubernetes", Name: "signer-ca"}, caSecret)
	if err != nil {
		signerFailure(r, csr, "CAFailure",
			fmt.Sprintf("Could not get CA certificate and key: %v", err))
		return reconcile.Result{}, err
	}

	// Decode the certificate request from PEM format.
	certReq, err := decodeCertificateRequest(csr.Spec.Request)
	if err != nil {
		// We dont degrade the status of the controller as this is due to a
		// malformed CSR rather than an issue with the controller.
		updateCSRStatusConditions(r, csr, "CSRDecodeFailure",
			fmt.Sprintf("Could not decode Certificate Request: %v", err))
		return reconcile.Result{}, nil
	}

	// Decode the CA certificate from PEM format.
	caCert, err := decodeCertificate(caSecret.Data["tls.crt"])
	if err != nil {
		signerFailure(r, csr, "CorruptCACert",
			fmt.Sprintf("Unable to decode CA certificate for %v: %v", signerName, err))
		return reconcile.Result{}, nil
	}

	// Decode the CA key from PEM format.
	caKey, err := decodePrivateKey(caSecret.Data["tls.key"])
	if err != nil {
		signerFailure(r, csr, "CorruptCAKey",
			fmt.Sprintf("Unable to decode CA private key for %v: %v", signerName, err))
		return reconcile.Result{}, nil
	}

	// Create a new certificate using the certificate template and certificate.
	// We can then sign this using the CA.
	signedCert, err := signCSR(newCertificateTemplate(certReq), certReq.PublicKey, caCert, caKey)
	if err != nil {
		signerFailure(r, csr, "SigningFailure",
			fmt.Sprintf("Unable to sign certificate for %v and signer %v: %v", request.Name, signerName, err))
		return reconcile.Result{}, nil
	}

	// Encode the certificate into PEM format and add to the status of the CSR
	csr.Status.Certificate, err = crypto.EncodeCertificates(signedCert)
	if err != nil {
		signerFailure(r, csr, "EncodeFailure",
			fmt.Sprintf("Could not encode certificate: %v", err))
		return reconcile.Result{}, nil
	}

	err = r.client.Status().Update(ctx, csr)
	if err != nil {
		log.Printf("Unable to update signed certificate for %v and signer %v: %v", request.Name, signerName, err)
		return reconcile.Result{}, err
	}

	log.Printf("Certificate signed, issued and approved for %s by %s", request.Name, signerName)
	r.status.SetNotDegraded(statusmanager.CertificateSigner)
	return reconcile.Result{}, nil
}

// isCertificateRequestApproved returns true if a certificate request has the
// "Approved" condition and no "Denied" conditions; false otherwise.
func isCertificateRequestApproved(csr *csrv1.CertificateSigningRequest) bool {
	approved, denied := getCertApprovalCondition(&csr.Status)
	return approved && !denied
}

func getCertApprovalCondition(status *csrv1.CertificateSigningRequestStatus) (approved bool, denied bool) {
	for _, c := range status.Conditions {
		if c.Type == csrv1.CertificateApproved {
			approved = true
		}
		if c.Type == csrv1.CertificateDenied {
			denied = true
		}
	}
	return
}

// Something has gone wrong with the signer controller so we update the statusmanager, the csr
// and log.
func signerFailure(r *ReconcileCSR, csr *csrv1.CertificateSigningRequest, reason string, message string) {
	log.Printf("%s: %s", reason, message)
	updateCSRStatusConditions(r, csr, reason, message)
	r.status.SetDegraded(statusmanager.CertificateSigner, reason, message)
}

// Update the status conditions on the CSR object
func updateCSRStatusConditions(r *ReconcileCSR, csr *csrv1.CertificateSigningRequest, reason string, message string) {
	csr.Status.Conditions = append(csr.Status.Conditions, csrv1.CertificateSigningRequestCondition{
		Type:    csrv1.CertificateFailed,
		Status:  "True",
		Reason:  reason,
		Message: message})

	err := r.client.Status().Update(context.TODO(), csr)
	if err != nil {
		log.Printf("Could not update CSR status: %v", err)
		r.status.SetDegraded(statusmanager.CertificateSigner, "UpdateFailure",
			fmt.Sprintf("Unable to update csr: %v", err))
	}
}

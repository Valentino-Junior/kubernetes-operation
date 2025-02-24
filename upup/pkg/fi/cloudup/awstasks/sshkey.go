/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package awstasks

import (
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"k8s.io/klog/v2"
	"k8s.io/kops/upup/pkg/fi/cloudup/terraformWriter"

	"k8s.io/kops/pkg/pki"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/awsup"
	"k8s.io/kops/upup/pkg/fi/cloudup/terraform"
)

// +kops:fitask
type SSHKey struct {
	ID        *string
	Name      *string
	Lifecycle fi.Lifecycle
	Shared    bool

	PublicKey fi.Resource

	KeyFingerprint *string

	Tags map[string]string
}

var _ fi.CompareWithID = &SSHKey{}
var _ fi.TaskNormalize = &SSHKey{}

func (e *SSHKey) CompareWithID() *string {
	return e.Name
}

func (e *SSHKey) Find(c *fi.Context) (*SSHKey, error) {
	cloud := c.Cloud.(awsup.AWSCloud)

	return e.find(cloud)
}

func (e *SSHKey) find(cloud awsup.AWSCloud) (*SSHKey, error) {
	request := &ec2.DescribeKeyPairsInput{
		KeyNames: []*string{e.Name},
	}

	response, err := cloud.EC2().DescribeKeyPairs(request)
	if awsErr, ok := err.(awserr.Error); ok {
		if awsErr.Code() == "InvalidKeyPair.NotFound" {
			err = nil
		}
	}
	if err != nil {
		return nil, fmt.Errorf("error listing SSHKeys: %v", err)
	}

	if response == nil || len(response.KeyPairs) == 0 {
		if e.IsExistingKey() && *e.Name != "" {
			return nil, fmt.Errorf("unable to find specified SSH key %q", *e.Name)
		}
		return nil, nil
	}

	if len(response.KeyPairs) != 1 {
		return nil, fmt.Errorf("Found multiple SSHKeys with Name %q", *e.Name)
	}

	k := response.KeyPairs[0]
	actual := &SSHKey{
		ID:             k.KeyPairId,
		Name:           k.KeyName,
		KeyFingerprint: k.KeyFingerprint,
		Tags:           mapEC2TagsToMap(k.Tags),
		Shared:         e.Shared,
	}

	// Avoid spurious changes
	if fi.ValueOf(k.KeyType) == ec2.KeyTypeEd25519 {
		// Trim the trailing "=" and prefix with "SHA256:" to match the output of "ssh-keygen -lf"
		fingerprint := fi.ValueOf(k.KeyFingerprint)
		fingerprint = strings.TrimRight(fingerprint, "=")
		fingerprint = fmt.Sprintf("SHA256:%s", fingerprint)
		actual.KeyFingerprint = fi.PtrTo(fingerprint)
	}
	if fi.ValueOf(actual.KeyFingerprint) == fi.ValueOf(e.KeyFingerprint) {
		klog.V(2).Infof("SSH key fingerprints match; assuming public keys match")
		actual.PublicKey = e.PublicKey
	} else {
		klog.V(2).Infof("Computed SSH key fingerprint mismatch: %q %q", fi.ValueOf(e.KeyFingerprint), fi.ValueOf(actual.KeyFingerprint))
	}
	actual.Lifecycle = e.Lifecycle
	if actual.Shared {
		// Don't report tag changes on shared keys
		actual.Tags = e.Tags
	}

	e.ID = actual.ID
	if e.IsExistingKey() && *e.Name != "" {
		e.KeyFingerprint = actual.KeyFingerprint
	}
	return actual, nil
}

func (e *SSHKey) Normalize(c *fi.Context) error {
	if e.KeyFingerprint == nil && e.PublicKey != nil {
		publicKey, err := fi.ResourceAsString(e.PublicKey)
		if err != nil {
			return fmt.Errorf("error reading SSH public key: %v", err)
		}

		keyFingerprint, err := pki.ComputeAWSKeyFingerprint(publicKey)
		if err != nil {
			return fmt.Errorf("error computing key fingerprint for SSH key: %v", err)
		}
		klog.V(2).Infof("Computed SSH key fingerprint as %q", keyFingerprint)
		e.KeyFingerprint = &keyFingerprint
	}

	return nil
}

func (e *SSHKey) Run(c *fi.Context) error {
	return fi.DefaultDeltaRunMethod(e, c)
}

func (s *SSHKey) CheckChanges(a, e, changes *SSHKey) error {
	if a != nil {
		if changes.Name != nil {
			return fi.CannotChangeField("Name")
		}
	}
	return nil
}

func (e *SSHKey) createKeypair(cloud awsup.AWSCloud) error {
	klog.V(2).Infof("Creating SSHKey with Name:%q", *e.Name)

	request := &ec2.ImportKeyPairInput{
		KeyName:           e.Name,
		TagSpecifications: awsup.EC2TagSpecification(ec2.ResourceTypeKeyPair, e.Tags),
	}

	if e.PublicKey != nil {
		d, err := fi.ResourceAsBytes(e.PublicKey)
		if err != nil {
			return fmt.Errorf("error rendering SSHKey PublicKey: %v", err)
		}
		request.PublicKeyMaterial = d
	}

	response, err := cloud.EC2().ImportKeyPair(request)
	if err != nil {
		return fmt.Errorf("error creating SSHKey: %v", err)
	}

	e.KeyFingerprint = response.KeyFingerprint
	e.ID = response.KeyPairId

	return nil
}

func (_ *SSHKey) RenderAWS(t *awsup.AWSAPITarget, a, e, changes *SSHKey) error {
	if a == nil {
		return e.createKeypair(t.Cloud)
	}

	if !e.Shared {
		return t.AddAWSTags(*e.ID, e.Tags)
	}
	return nil
}

type terraformSSHKey struct {
	Name      *string                  `cty:"key_name"`
	PublicKey *terraformWriter.Literal `cty:"public_key"`
	Tags      map[string]string        `cty:"tags"`
}

func (_ *SSHKey) RenderTerraform(t *terraform.TerraformTarget, a, e, changes *SSHKey) error {
	// We don't want to render a key definition when we're using one that already exists
	if e.IsExistingKey() {
		return nil
	}
	tfName := strings.Replace(*e.Name, ":", "", -1)
	publicKey, err := t.AddFileResource("aws_key_pair", tfName, "public_key", e.PublicKey, false)
	if err != nil {
		return fmt.Errorf("error rendering PublicKey: %v", err)
	}

	tf := &terraformSSHKey{
		Name:      e.Name,
		PublicKey: publicKey,
		Tags:      e.Tags,
	}

	return t.RenderResource("aws_key_pair", tfName, tf)
}

// IsExistingKey will be true if the task has been initialized without using a public key
// this is when we want to use a key that is already present in AWS.
func (e *SSHKey) IsExistingKey() bool {
	return e.PublicKey == nil
}

func (e *SSHKey) TerraformLink() *terraformWriter.Literal {
	if e.NoSSHKey() {
		return nil
	}
	if e.IsExistingKey() {
		return terraformWriter.LiteralFromStringValue(*e.Name)
	}
	tfName := strings.Replace(*e.Name, ":", "", -1)
	return terraformWriter.LiteralProperty("aws_key_pair", tfName, "id")
}

func (e *SSHKey) NoSSHKey() bool {
	return e.ID == nil && e.Name == nil && e.PublicKey == nil && e.KeyFingerprint == nil
}

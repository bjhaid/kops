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
	"encoding/json"
	"fmt"
	"strconv"

	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sqs"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/awsup"
	"k8s.io/kops/upup/pkg/fi/cloudup/cloudformation"
	"k8s.io/kops/upup/pkg/fi/cloudup/terraform"
)

// +kops:fitask
type SQS struct {
	Name      *string
	Lifecycle *fi.Lifecycle

	URL                    *string
	MessageRetentionPeriod int
	Policy                 fi.Resource // "inline" IAM policy

	Tags map[string]string
}

var _ fi.CompareWithID = &SQS{}

func (q *SQS) CompareWithID() *string {
	return q.URL
}

func (q *SQS) Find(c *fi.Context) (*SQS, error) {
	cloud := c.Cloud.(awsup.AWSCloud)

	if q.Name == nil {
		return nil, nil
	}

	response, err := cloud.SQS().ListQueues(&sqs.ListQueuesInput{
		MaxResults:      aws.Int64(2),
		QueueNamePrefix: q.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("error listing SQS queues: %v", err)
	}
	if response == nil || len(response.QueueUrls) == 0 {
		return nil, nil
	}
	if len(response.QueueUrls) != 1 {
		return nil, fmt.Errorf("found multiple SQS queues matching queue name")
	}
	url := response.QueueUrls[0]

	attributes, err := cloud.SQS().GetQueueAttributes(&sqs.GetQueueAttributesInput{
		AttributeNames: []*string{s("MessageRetentionPeriod"), s("Policy")},
		QueueUrl:       url,
	})
	if err != nil {
		return nil, fmt.Errorf("error getting SQS queue attributes: %v", err)
	}
	policy := fi.NewStringResource(*attributes.Attributes["Policy"])
	period, err := strconv.Atoi(*attributes.Attributes["MessageRetentionPeriod"])
	if err != nil {
		return nil, fmt.Errorf("error coverting MessageRetentionPeriod to int: %v", err)
	}

	tags, err := cloud.SQS().ListQueueTags(&sqs.ListQueueTagsInput{
		QueueUrl: url,
	})
	if err != nil {
		return nil, fmt.Errorf("error listing SQS queue tags: %v", err)
	}

	actual := &SQS{
		Name:                   q.Name,
		URL:                    url,
		Lifecycle:              q.Lifecycle,
		Policy:                 policy,
		MessageRetentionPeriod: period,
		Tags:                   intersectSQSTags(tags.Tags, q.Tags),
	}

	return actual, nil
}

func (q *SQS) Run(c *fi.Context) error {
	return fi.DefaultDeltaRunMethod(q, c)
}

func (q *SQS) CheckChanges(a, e, changes *SQS) error {
	if a == nil {
		if e.Name == nil {
			return field.Required(field.NewPath("Name"), "")
		}
	}
	if a != nil {
		if changes.URL != nil {
			return fi.CannotChangeField("URL")
		}
	}
	return nil
}

func (q *SQS) RenderAWS(t *awsup.AWSAPITarget, a, e, changes *SQS) error {
	policy, err := fi.ResourceAsString(e.Policy)
	if err != nil {
		return fmt.Errorf("error rendering RolePolicyDocument: %v", err)
	}

	if a == nil {
		request := &sqs.CreateQueueInput{
			Attributes: map[string]*string{
				"MessageRetentionPeriod": s(strconv.Itoa(q.MessageRetentionPeriod)),
				"Policy":                 s(policy),
			},
			QueueName: q.Name,
			Tags:      convertTagsToPointers(q.Tags),
		}
		response, err := t.Cloud.SQS().CreateQueue(request)
		if err != nil {
			return fmt.Errorf("error creating SQS queue: %v", err)
		}

		q.URL = response.QueueUrl
	}

	return nil
}

type terraformSQSQueue struct {
	Name                    *string            `json:"name" cty:"name"`
	MessageRetentionSeconds int                `json:"message_retention_seconds" cty:"message_retention_seconds"`
	Policy                  *terraform.Literal `json:"policy" cty:"policy"`
	Tags                    map[string]string  `json:"tags" cty:"tags"`
}

func (_ *SQS) RenderTerraform(t *terraform.TerraformTarget, a, e, changes *SQS) error {
	p, err := t.AddFile("aws_sqs_queue", *e.Name, "policy", e.Policy, false)
	if err != nil {
		return err
	}

	tf := &terraformSQSQueue{
		Name:                    e.Name,
		MessageRetentionSeconds: e.MessageRetentionPeriod,
		Policy:                  p,
		Tags:                    e.Tags,
	}

	return t.RenderResource("aws_sqs_queue", *e.Name, tf)
}

type cloudformationSQSQueue struct {
	QueueName              *string             `json:"QueueName"`
	MessageRetentionPeriod int                 `json:"MessageRetentionPeriod"`
	Tags                   []cloudformationTag `json:"Tags,omitempty"`
}

type cloudformationSQSQueuePolicy struct {
	Queues         []*cloudformation.Literal `json:"Queues"`
	PolicyDocument map[string]interface{}    `json:"PolicyDocument"`
	Tags           []cloudformationTag       `json:"Tags,omitempty"`
}

func (_ *SQS) RenderCloudformation(t *cloudformation.CloudformationTarget, a, e, changes *SQS) error {
	cfQueue := &cloudformationSQSQueue{
		QueueName:              e.Name,
		MessageRetentionPeriod: e.MessageRetentionPeriod,
		Tags:                   buildCloudformationTags(e.Tags),
	}

	err := t.RenderResource("AWS::SQS::Queue", *e.Name, cfQueue)
	if err != nil {
		return err
	}

	// convert Policy string into json
	jsonString, err := fi.ResourceAsBytes(e.Policy)
	if err != nil {
		return err
	}
	data := make(map[string]interface{})
	err = json.Unmarshal(jsonString, &data)
	if err != nil {
		return fmt.Errorf("error parsing SQS PolicyDocument: %v", err)
	}

	cfQueueRef := cloudformation.Ref("AWS::SQS::Queue", fi.StringValue(e.Name))

	cfQueuePolicy := &cloudformationSQSQueuePolicy{
		Queues:         []*cloudformation.Literal{cfQueueRef},
		PolicyDocument: data,
	}
	return t.RenderResource("AWS::SQS::QueuePolicy", *e.Name+"Policy", cfQueuePolicy)
}

// change tags to format required by CreateQueue
func convertTagsToPointers(tags map[string]string) map[string]*string {
	newTags := map[string]*string{}
	for k, v := range tags {
		vv := v
		newTags[k] = &vv
	}

	return newTags
}

// intersectSQSTags does the same thing as intersectTags, but takes different input because SQS tags are listed differently
func intersectSQSTags(tags map[string]*string, desired map[string]string) map[string]string {
	if tags == nil {
		return nil
	}
	actual := make(map[string]string)
	for k, v := range tags {
		vv := aws.StringValue(v)

		if _, found := desired[k]; found {
			actual[k] = vv
		}
	}
	if len(actual) == 0 && desired == nil {
		// Avoid problems with comparison between nil & {}
		return nil
	}
	return actual
}

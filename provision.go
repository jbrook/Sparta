// +build !lambdabinary

// Install aws-sdk
//go:generate rm -rf ./node_modules
//go:generate npm install aws-sdk --prefix ./
// There's a handful of subdirectories that we don't need at runtime...
//go:generate rm -rf ./node_modules/aws-sdk/dist/
//go:generate rm -rf ./node_modules/aws-sdk/dist-tools/
// Zip up the modules
//go:generate zip -vr ./resources/provision/node_modules.zip ./node_modules/
//go:generate rm -rf ./node_modules

// Embed the custom service handlers
// TODO: Move these into golang
//go:generate go run ./vendor/github.com/mweagle/esc/main.go -o ./CONSTANTS.go -private -pkg sparta ./resources

// cleanup
//go:generate rm -f ./resources/provision/node_modules.zip

package sparta

import (
	"archive/zip"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
)

const (
	// OutputSpartaHomeKey is the keyname used in the CloudFormation Output
	// that stores the Sparta home URL.
	OutputSpartaHomeKey = "SpartaHome"

	// OutputSpartaVersionKey is the keyname used in the CloudFormation Output
	// that stores the Sparta version used to provision/update the service.
	OutputSpartaVersionKey = "SpartaVersion"
)

var customResourceScripts = []string{"cfn-response.js",
	"underscore-min.js",
	"async.min.js",
	"apigateway.js",
	"s3.js",
	"sns.js",
	"golang-constants.json"}

type workflowContext struct {
	noop                    bool
	serviceName             string
	serviceDescription      string
	lambdaAWSInfos          []*LambdaAWSInfo
	api                     *API
	cloudformationResources ArbitraryJSONObject
	cloudformationOutputs   ArbitraryJSONObject
	lambdaIAMRoleNameMap    map[string]interface{}
	s3Bucket                string
	s3LambdaZipKey          string
	awsSession              *session.Session
	templateWriter          io.Writer
	logger                  *logrus.Logger
}

type customResourceManager struct {
}

type workflowStep func(ctx *workflowContext) (workflowStep, error)

// Verify & cache the IAM rolename to ARN mapping
func verifyIAMRoles(ctx *workflowContext) (workflowStep, error) {
	// The map is either a literal Arn from a pre-existing role name
	// or a ArbitraryJSONObject{
	// 	"Fn::GetAtt": []string{iamRoleDefinitionName, "Arn"},
	// }

	// Don't verify them, just create them...
	ctx.logger.Info("Verifying IAM Lambda execution roles")
	ctx.lambdaIAMRoleNameMap = make(map[string]interface{}, 0)
	svc := iam.New(ctx.awsSession)

	for _, eachLambda := range ctx.lambdaAWSInfos {
		if "" != eachLambda.RoleName && nil != eachLambda.RoleDefinition {
			return nil, fmt.Errorf("Both RoleName and RoleDefinition defined for lambda: %s", eachLambda.lambdaFnName)
		}

		// Get the IAM role name
		if "" != eachLambda.RoleName {
			_, exists := ctx.lambdaIAMRoleNameMap[eachLambda.RoleName]
			if !exists {
				// Check the role
				params := &iam.GetRoleInput{
					RoleName: aws.String(eachLambda.RoleName),
				}
				ctx.logger.Debug("Checking IAM RoleName: ", eachLambda.RoleName)
				resp, err := svc.GetRole(params)
				if err != nil {
					ctx.logger.Error(err.Error())
					return nil, err
				}
				// Cache it - we'll need it later when we create the
				// CloudFormation template which needs the execution Arn (not role)
				ctx.lambdaIAMRoleNameMap[eachLambda.RoleName] = *resp.Role.Arn
			}
		} else {
			logicalName := eachLambda.RoleDefinition.logicalName()
			_, exists := ctx.lambdaIAMRoleNameMap[logicalName]
			if !exists {
				// Insert it into the resource creation map and add
				// the "Ref" entry to the hashmap
				ctx.cloudformationResources[logicalName] = eachLambda.RoleDefinition.rolePolicy(eachLambda.EventSourceMappings, ctx.logger)

				ctx.lambdaIAMRoleNameMap[logicalName] = ArbitraryJSONObject{
					"Fn::GetAtt": []string{logicalName, "Arn"},
				}
			}
		}
	}
	ctx.logger.Info("IAM roles verified. Count: ", len(ctx.lambdaIAMRoleNameMap))
	return createPackageStep(), nil
}

// Return a string representation of a JS function call that can be exposed
// to AWS Lambda
func createNewNodeJSProxyEntry(lambdaInfo *LambdaAWSInfo, logger *logrus.Logger) string {
	// Create an entry of the form:
	logger.Info("Creating NodeJS proxy entry: " + lambdaInfo.jsHandlerName())
	primaryEntry := fmt.Sprintf("exports[\"%s\"] = createForwarder(\"/%s\");\n",
		lambdaInfo.jsHandlerName(),
		lambdaInfo.lambdaFnName)
	return primaryEntry
}

// Return the StackEvents for the given StackName/StackID
func stackEvents(stackID string, cfService *cloudformation.CloudFormation) ([]*cloudformation.StackEvent, error) {
	var events []*cloudformation.StackEvent

	nextToken := ""
	for {
		params := &cloudformation.DescribeStackEventsInput{
			StackName: aws.String(stackID),
		}
		if len(nextToken) > 0 {
			params.NextToken = aws.String(nextToken)
		}

		resp, err := cfService.DescribeStackEvents(params)
		if nil != err {
			return nil, err
		}
		events = append(events, resp.StackEvents...)
		if nil == resp.NextToken {
			break
		} else {
			nextToken = *resp.NextToken
		}
	}
	return events, nil
}

// Build and package the application
func createPackageStep() workflowStep {

	return func(ctx *workflowContext) (workflowStep, error) {
		// Compile the source to linux...
		sanitizedServiceName := sanitizedName(ctx.serviceName)
		executableOutput := fmt.Sprintf("%s.lambda.amd64", sanitizedServiceName)
		cmd := exec.Command("go", "build", "-o", executableOutput, "-tags", "lambdabinary", ".")
		ctx.logger.Debug("Building application binary: ", cmd.Args)
		cmd.Env = os.Environ()
		cmd.Env = append(cmd.Env, "GOOS=linux", "GOARCH=amd64", "GO15VENDOREXPERIMENT=1")
		ctx.logger.Info("Compiling binary: ", executableOutput)

		outputWriter := ctx.logger.Writer()
		defer outputWriter.Close()
		cmd.Stdout = outputWriter
		cmd.Stderr = outputWriter

		err := cmd.Run()
		if err != nil {
			return nil, err
		}
		defer os.Remove(executableOutput)

		// Binary size
		stat, err := os.Stat(executableOutput)
		if err != nil {
			return nil, errors.New("Failed to stat build output")
		}
		// Minimum hello world size is 2.3M
		// Minimum HTTP hello world is 6.3M
		ctx.logger.Info("Executable binary size (MB): ", stat.Size()/(1024*1024))

		workingDir, err := os.Getwd()
		if err != nil {
			return nil, errors.New("Failed to retrieve working directory")
		}
		tmpFile, err := ioutil.TempFile(workingDir, sanitizedServiceName)
		if err != nil {
			return nil, errors.New("Failed to create temporary file")
		}

		defer func() {
			tmpFile.Close()
		}()

		ctx.logger.Info("Creating ZIP archive for upload: ", tmpFile.Name())
		lambdaArchive := zip.NewWriter(tmpFile)
		defer lambdaArchive.Close()

		// File info for the binary executable
		binaryWriter, err := lambdaArchive.Create(filepath.Base(executableOutput))
		if err != nil {
			return nil, fmt.Errorf("Failed to create ZIP entry: %s", filepath.Base(executableOutput))
		}
		reader, err := os.Open(executableOutput)
		if err != nil {
			return nil, fmt.Errorf("Failed to open file: %s", executableOutput)
		}
		defer reader.Close()
		io.Copy(binaryWriter, reader)

		// Add the string literal adapter, which requires us to add exported
		// functions to the end of index.js
		nodeJSWriter, err := lambdaArchive.Create("index.js")
		if err != nil {
			return nil, errors.New("Failed to create ZIP entry: index.js")
		}
		nodeJSSource := _escFSMustString(false, "/resources/index.js")
		nodeJSSource += "\n// DO NOT EDIT - CONTENT UNTIL EOF IS AUTOMATICALLY GENERATED\n"
		for _, eachLambda := range ctx.lambdaAWSInfos {
			nodeJSSource += createNewNodeJSProxyEntry(eachLambda, ctx.logger)
		}
		// Finally, replace
		// 	SPARTA_BINARY_NAME = 'Sparta.lambda.amd64';
		// with the service binary name
		nodeJSSource += fmt.Sprintf("SPARTA_BINARY_NAME='%s';\n", executableOutput)
		ctx.logger.Debug("Dynamically generated NodeJS adapter:\n", nodeJSSource)
		stringReader := strings.NewReader(nodeJSSource)
		io.Copy(nodeJSWriter, stringReader)

		// Also embed the custom resource creation scripts
		for _, eachName := range customResourceScripts {
			resourceName := fmt.Sprintf("/resources/provision/%s", eachName)
			resourceContent := _escFSMustString(false, resourceName)
			stringReader := strings.NewReader(resourceContent)
			embedWriter, err := lambdaArchive.Create(eachName)
			if nil != err {
				return nil, err
			}
			ctx.logger.Info("Embedding CustomResource script: ", eachName)
			io.Copy(embedWriter, stringReader)
		}

		// And finally, if there is a node_modules.zip file, then include it.
		nodeModuleBytes, err := _escFSByte(false, "/resources/provision/node_modules.zip")
		if nil == err {
			nodeModuleReader, err := zip.NewReader(bytes.NewReader(nodeModuleBytes), int64(len(nodeModuleBytes)))
			if err != nil {
				return nil, err
			}
			for _, zipFile := range nodeModuleReader.File {
				embedWriter, err := lambdaArchive.Create(zipFile.Name)
				if nil != err {
					return nil, err
				}
				ctx.logger.Debug("Copying node_module file: ", zipFile.Name)
				sourceReader, err := zipFile.Open()
				if err != nil {
					return nil, err
				}
				io.Copy(embedWriter, sourceReader)
			}
		} else {
			ctx.logger.Warn("Failed to load /resources/provision/node_modules.zip for embedding", err)
		}
		return createUploadStep(tmpFile.Name()), nil
	}
}

// Upload the ZIP archive to S3
func createUploadStep(packagePath string) workflowStep {
	return func(ctx *workflowContext) (workflowStep, error) {
		reader, err := os.Open(packagePath)
		if err != nil {
			return nil, fmt.Errorf("Failed to open local archive for S3 upload: %s", err.Error())
		}
		defer func() {
			reader.Close()
			os.Remove(packagePath)
		}()

		body, err := os.Open(packagePath)
		if nil != err {
			return nil, err
		}
		keyName := filepath.Base(packagePath)
		// Cache it in case there was an error & we need to cleanup
		ctx.s3LambdaZipKey = keyName
		uploadInput := &s3manager.UploadInput{
			Bucket:      &ctx.s3Bucket,
			Key:         &keyName,
			ContentType: aws.String("application/zip"),
			Body:        body,
		}

		if ctx.noop {
			ctx.logger.WithFields(logrus.Fields{
				"Bucket": ctx.s3Bucket,
				"Key":    keyName,
			}).Info("Bypassing S3 ZIP upload due to -n/-noop command line argument")
		} else {
			ctx.logger.Info("Uploading ZIP archive to S3")
			uploader := s3manager.NewUploader(session.New())
			result, err := uploader.Upload(uploadInput)
			if nil != err {
				return nil, err
			}
			ctx.logger.Info("ZIP archive uploaded: ", result.Location)
		}
		return ensureCloudFormationStack(keyName), nil
	}
}

// Does a given stack exist?
func stackExists(stackNameOrID string, cf *cloudformation.CloudFormation, logger *logrus.Logger) (bool, error) {
	describeStacksInput := &cloudformation.DescribeStacksInput{
		StackName: aws.String(stackNameOrID),
	}
	describeStacksOutput, err := cf.DescribeStacks(describeStacksInput)
	logger.Debug("DescribeStackOutput: ", describeStacksOutput)
	exists := false
	if err != nil {
		logger.Info("DescribeStackOutputError: ", err)
		// If the stack doesn't exist, then no worries
		if strings.Contains(err.Error(), "does not exist") {
			exists = false
		} else {
			return false, err
		}
	} else {
		exists = true
	}
	return exists, nil
}

// TODO: Replace this with the implementation
// provided by vendor/github.com/aws/aws-sdk-go/service/cloudformation/waiters.go
func convergeStackState(cfTemplateURL string, ctx *workflowContext) (*cloudformation.Stack, error) {
	awsCloudFormation := cloudformation.New(ctx.awsSession)

	// Does it exist?
	exists, err := stackExists(ctx.serviceName, awsCloudFormation, ctx.logger)
	if nil != err {
		return nil, err
	}
	stackID := ""
	if exists {
		// Update stack
		updateStackInput := &cloudformation.UpdateStackInput{
			StackName:    aws.String(ctx.serviceName),
			TemplateURL:  aws.String(cfTemplateURL),
			Capabilities: []*string{aws.String("CAPABILITY_IAM")},
		}
		updateStackResponse, err := awsCloudFormation.UpdateStack(updateStackInput)
		if nil != err {
			return nil, err
		}
		ctx.logger.Info("Issued update request: ", *updateStackResponse.StackId)
		stackID = *updateStackResponse.StackId
	} else {
		// Create stack
		createStackInput := &cloudformation.CreateStackInput{
			StackName:        aws.String(ctx.serviceName),
			TemplateURL:      aws.String(cfTemplateURL),
			TimeoutInMinutes: aws.Int64(5),
			OnFailure:        aws.String(cloudformation.OnFailureDelete),
			Capabilities:     []*string{aws.String("CAPABILITY_IAM")},
		}
		createStackResponse, err := awsCloudFormation.CreateStack(createStackInput)
		if nil != err {
			return nil, err
		}
		ctx.logger.Info("Creating stack: ", *createStackResponse.StackId)
		stackID = *createStackResponse.StackId
	}

	// Poll for the current stackID state
	describeStacksInput := &cloudformation.DescribeStacksInput{
		StackName: aws.String(stackID),
	}

	var stackInfo *cloudformation.Stack
	stackOperationComplete := false
	ctx.logger.Info("Waiting for stack to complete")
	for !stackOperationComplete {
		time.Sleep(10 * time.Second)
		describeStacksOutput, err := awsCloudFormation.DescribeStacks(describeStacksInput)
		if nil != err {
			return nil, err
		}
		if len(describeStacksOutput.Stacks) > 0 {
			stackInfo = describeStacksOutput.Stacks[0]
			ctx.logger.Info("Current state: ", *stackInfo.StackStatus)
			switch *stackInfo.StackStatus {
			case cloudformation.StackStatusCreateInProgress,
				cloudformation.StackStatusDeleteInProgress,
				cloudformation.StackStatusUpdateInProgress,
				cloudformation.StackStatusRollbackInProgress,
				cloudformation.StackStatusUpdateCompleteCleanupInProgress,
				cloudformation.StackStatusUpdateRollbackCompleteCleanupInProgress,
				cloudformation.StackStatusUpdateRollbackInProgress:
				time.Sleep(20 * time.Second)
			default:
				stackOperationComplete = true
				break
			}
		} else {
			return nil, fmt.Errorf("More than one stack returned for: %s", stackID)
		}
	}
	// What happened?
	succeed := true
	switch *stackInfo.StackStatus {
	case cloudformation.StackStatusDeleteComplete, // Initial create failure
		cloudformation.StackStatusUpdateRollbackComplete: // Update failure
		succeed = false
	default:
		succeed = true
	}

	// If it didn't work, then output some failure information
	if !succeed {
		// Get the stack events and find the ones that failed.
		events, err := stackEvents(stackID, awsCloudFormation)
		if nil != err {
			return nil, err
		}
		ctx.logger.Error("Stack provisioning failed.")
		for _, eachEvent := range events {
			switch *eachEvent.ResourceStatus {
			case cloudformation.ResourceStatusCreateFailed,
				cloudformation.ResourceStatusDeleteFailed,
				cloudformation.ResourceStatusUpdateFailed:
				errMsg := fmt.Sprintf("\tError ensuring %s (%s): %s",
					*eachEvent.ResourceType,
					*eachEvent.LogicalResourceId,
					*eachEvent.ResourceStatusReason)
				ctx.logger.Error(errMsg)
			default:
				// NOP
			}
		}
		return nil, fmt.Errorf("Failed to provision: %s", ctx.serviceName)
	} else if nil != stackInfo.Outputs {
		ctx.logger.Info("Stack Outputs:")
		for _, eachOutput := range stackInfo.Outputs {
			ctx.logger.WithFields(logrus.Fields{
				"Key":         *eachOutput.OutputKey,
				"Value":       *eachOutput.OutputValue,
				"Description": *eachOutput.Description,
			}).Info("\tOutput")
		}
	}
	return stackInfo, nil
}

func ensureCloudFormationStack(s3Key string) workflowStep {
	return func(ctx *workflowContext) (workflowStep, error) {
		// We're going to create a template that represents the new state of the
		// lambda world.
		cloudFormationTemplate := ArbitraryJSONObject{
			"AWSTemplateFormatVersion": "2010-09-09",
			"Description":              ctx.serviceDescription,
		}
		for _, eachEntry := range ctx.lambdaAWSInfos {
			err := eachEntry.export(ctx.serviceName,
                              ctx.s3Bucket,
                              s3Key,
                              ctx.lambdaIAMRoleNameMap,
                              ctx.cloudformationResources,
                              ctx.cloudformationOutputs,
                              ctx.logger)
			if nil != err {
				return nil, err
			}
		}
		// If there's an API gateway definition, provision custom resources
		// and IAM role to
		if nil != ctx.api {
			ctx.api.export(ctx.s3Bucket, s3Key, ctx.lambdaIAMRoleNameMap, ctx.cloudformationResources, ctx.cloudformationOutputs, ctx.logger)
		}
		// Add Sparta outputs
		ctx.cloudformationOutputs[OutputSpartaVersionKey] = ArbitraryJSONObject{
			"Description": "Sparta Version",
			"Value":       SpartaVersion,
		}
		ctx.cloudformationOutputs[OutputSpartaHomeKey] = ArbitraryJSONObject{
			"Description": "Sparta Home",
			"Value":       "https://github.com/mweagle/Sparta",
		}
		cloudFormationTemplate["Resources"] = ctx.cloudformationResources
		cloudFormationTemplate["Outputs"] = ctx.cloudformationOutputs

		// Generate a complete CloudFormation template
		cfTemplate, err := json.Marshal(cloudFormationTemplate)
		if err != nil {
			ctx.logger.Error("Failed to Marshal CloudFormation template: ", err.Error())
			return nil, err
		}

		// Upload the template to S3
		contentBody := string(cfTemplate)
		sanitizedServiceName := sanitizedName(ctx.serviceName)
		hash := sha1.New()
		hash.Write([]byte(contentBody))
		s3keyName := fmt.Sprintf("%s-%s-cf.json", sanitizedServiceName, hex.EncodeToString(hash.Sum(nil)))

		uploadInput := &s3manager.UploadInput{
			Bucket:      &ctx.s3Bucket,
			Key:         &s3keyName,
			ContentType: aws.String("application/json"),
			Body:        strings.NewReader(contentBody),
		}
		formatted, err := json.MarshalIndent(contentBody, "", " ")
		if nil != err {
			return nil, err
		}
		ctx.logger.Debug("CloudFormation template body: ", string(formatted))
		if nil != ctx.templateWriter {
			io.WriteString(ctx.templateWriter, string(formatted))
		}

		if ctx.noop {
			ctx.logger.WithFields(logrus.Fields{
				"Bucket": ctx.s3Bucket,
				"Key":    s3keyName,
			}).Info("Bypassing template upload & creation due to -n/-noop command line argument")
		} else {
			ctx.logger.Info("Uploading CloudFormation template")
			uploader := s3manager.NewUploader(ctx.awsSession)
			templateUploadResult, err := uploader.Upload(uploadInput)
			if nil != err {
				return nil, err
			}
			ctx.logger.Info("CloudFormation template uploaded: ", templateUploadResult.Location)
			stack, err := convergeStackState(templateUploadResult.Location, ctx)
			if nil != err {
				return nil, err
			}
			ctx.logger.Info("Stack provisioned: ", stack)
		}
		return nil, nil
	}
}

// Provision compiles, packages, and provisions (either via create or update) a Sparta application.
// The serviceName is the service's logical
// identify and is used to determine create vs update operations.  The compilation options/flags are:
//
// 	TAGS:         -tags lambdabinary
// 	ENVIRONMENT:  GOOS=linux GOARCH=amd64 GO15VENDOREXPERIMENT=1
//
// The compiled binary is packaged with a NodeJS proxy shim to manage AWS Lambda setup & invocation per
// http://docs.aws.amazon.com/lambda/latest/dg/authoring-function-in-nodejs.html
//
// The two files are ZIP'd, posted to S3 and used as an input to a dynamically generated CloudFormation
// template (http://docs.aws.amazon.com/AWSCloudFormation/latest/UserGuide/Welcome.html)
// which creates or updates the service state.
//
// More information on golang 1.5's support for vendor'd resources is documented at
//
//  https://docs.google.com/document/d/1Bz5-UB7g2uPBdOx-rw5t9MxJwkfpx90cqG9AFL0JAYo/edit
//  https://medium.com/@freeformz/go-1-5-s-vendor-experiment-fd3e830f52c3#.voiicue1j
//
// type Configuration struct {
//     Val   string
//     Proxy struct {
//         Address string
//         Port    string
//     }
// }
func Provision(noop bool,
	serviceName string,
	serviceDescription string,
	lambdaAWSInfos []*LambdaAWSInfo,
	api *API,
	s3Bucket string,
	templateWriter io.Writer,
	logger *logrus.Logger) error {

	ctx := &workflowContext{
		noop:               noop,
		serviceName:        serviceName,
		serviceDescription: serviceDescription,
		lambdaAWSInfos:     lambdaAWSInfos,
		api:                api,
		cloudformationResources: make(ArbitraryJSONObject, 0),
		cloudformationOutputs:   make(ArbitraryJSONObject, 0),
		s3Bucket:                s3Bucket,
		awsSession:              awsSession(logger),
		templateWriter:          templateWriter,
		logger:                  logger,
	}

	if len(lambdaAWSInfos) <= 0 {
		return errors.New("No lambda functions provided to Sparta.Provision()")
	}

	for step := verifyIAMRoles; step != nil; {
		next, err := step(ctx)
		if err != nil {
			ctx.logger.Error(err.Error())
			if "" != ctx.s3LambdaZipKey {
				ctx.logger.Info("Attempting to cleanup ZIP archive: ", ctx.s3LambdaZipKey)
				s3Client := s3.New(ctx.awsSession)
				params := &s3.DeleteObjectInput{
					Bucket: aws.String(ctx.s3Bucket),
					Key:    aws.String(ctx.s3LambdaZipKey),
				}
				_, err := s3Client.DeleteObject(params)
				if nil != err {
					ctx.logger.Warn("Failed to delete archive")
				}
			}
			return err
		}
		if next == nil {
			break
		} else {
			step = next
		}
	}
	return nil
}

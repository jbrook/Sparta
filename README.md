[![Build Status](https://travis-ci.org/mweagle/Sparta.svg?branch=master)](https://travis-ci.org/mweagle/Sparta)

# Sparta <p align="center">

<div align="center"><img src="https://raw.githubusercontent.com/mweagle/Sparta/master/site/SpartanShieldSmall.png" />
</div>

## Overview

Sparta takes a set of _golang_ functions and automatically provisions them in
[AWS Lambda](https://aws.amazon.com/lambda/) as a logical unit.

Functions must implement

    type LambdaFunction func(*json.RawMessage,
                              *LambdaContext,
                              http.ResponseWriter,
                              *logrus.Logger)

where

  * `json.RawMessage` :  The arbitrary `json.RawMessage` event data provided to the function.
  * `LambdaContext` : _golang_ compatible representation of the AWS Lambda [Context](http://docs.aws.amazon.com/lambda/latest/dg/nodejs-prog-model-context.html)
  * `http.ResponseWriter` : Writer for response. The HTTP status code & response body is translated to a pass/fail result provided to the `context.done()` handler.
  * `logrus.Logger` : [logrus](https://github.com/Sirupsen/logrus) logger with JSON output. See an [example](https://github.com/Sirupsen/logrus#example) for including JSON fields.

Given a set of registered _golang_ functions, Sparta will:

  * Either verify or provision the defined [IAM roles](http://docs.aws.amazon.com/lambda/latest/dg/intro-permission-model.html)
  * Build a deployable application via `Provision()`
  * Zip the contents and associated JS proxying logic
  * Dynamically create a CloudFormation template to either create or update the service state.
  * Optionally:
    * Register with S3 and SNS for push source configuration
    * Provision an [API Gateway](https://aws.amazon.com/api-gateway/) service to make your functions publicly available

Note that Lambda updates may be performed with [no interruption](http://docs.aws.amazon.com/AWSCloudFormation/latest/UserGuide/aws-resource-lambda-function.html)
in service.

Visit [gosparta.io](http://gosparta.io) for complete documentation.

## Limitations

See the [Limitations](http://gosparta.io/docs/limitations/) page for the most up-to-date information.

## Outstanding
  - Eliminate NodeJS CustomResources
  - Support API Gateway updates
    - Currently API reprovisioning is done by `delete` => `create`
  - Optimize _CONSTANTS.go_ for deployed binary
  - Implement APIGateway graph
  - Support APIGateway inline Model definition
  - Support custom domains

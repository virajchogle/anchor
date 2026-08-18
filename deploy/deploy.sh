#!/usr/bin/env bash
# Deploy Anchor to AWS Lambda behind a Function URL.
#
# One command, idempotent: safe to re-run. Creates the IAM role, the secret, the
# function, and the URL if they do not exist, and updates them if they do.
#
#   source ~/.anchor/env && ./deploy/deploy.sh
#
# Requires: AWS credentials with permission to manage Lambda, IAM, and Secrets
# Manager, plus ANCHOR_DB_URL for the cluster the agent reads and writes.
set -euo pipefail

FUNC="${ANCHOR_FUNCTION:-anchor-panel}"
REGION="${AWS_REGION:-us-east-1}"
SECRET="${ANCHOR_SECRET_NAME:-anchor/db-url}"
ROLE="${ANCHOR_ROLE:-anchor-lambda-role}"
RUNTIME="provided.al2023"

say() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

: "${ANCHOR_DB_URL:?set ANCHOR_DB_URL (source ~/.anchor/env)}"
aws sts get-caller-identity >/dev/null || { echo "AWS credentials are not working"; exit 1; }
ACCOUNT=$(aws sts get-caller-identity --query Account --output text)

say "Storing the database URL in Secrets Manager"
# The credential lives here rather than in a Lambda environment variable, where
# it would be visible in the console and in every describe-function call.
if aws secretsmanager describe-secret --secret-id "$SECRET" --region "$REGION" >/dev/null 2>&1; then
  aws secretsmanager put-secret-value --secret-id "$SECRET" \
    --secret-string "$ANCHOR_DB_URL" --region "$REGION" >/dev/null
  echo "updated $SECRET"
else
  aws secretsmanager create-secret --name "$SECRET" \
    --description "Anchor CockroachDB connection string" \
    --secret-string "$ANCHOR_DB_URL" --region "$REGION" >/dev/null
  echo "created $SECRET"
fi
SECRET_ARN=$(aws secretsmanager describe-secret --secret-id "$SECRET" --region "$REGION" --query ARN --output text)

say "Ensuring the execution role"
if ! aws iam get-role --role-name "$ROLE" >/dev/null 2>&1; then
  aws iam create-role --role-name "$ROLE" --assume-role-policy-document '{
    "Version":"2012-10-17",
    "Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}' >/dev/null
  aws iam attach-role-policy --role-name "$ROLE" \
    --policy-arn arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole >/dev/null
  echo "created role $ROLE"
  sleep 10   # IAM is eventually consistent; Lambda rejects the role otherwise
fi

# Least privilege: read exactly one secret, invoke exactly the two models we use.
aws iam put-role-policy --role-name "$ROLE" --policy-name anchor-runtime \
  --policy-document "{
    \"Version\":\"2012-10-17\",
    \"Statement\":[
      {\"Effect\":\"Allow\",\"Action\":[\"secretsmanager:GetSecretValue\"],\"Resource\":\"$SECRET_ARN\"},
      {\"Effect\":\"Allow\",\"Action\":[\"bedrock:InvokeModel\"],\"Resource\":[
         \"arn:aws:bedrock:$REGION::foundation-model/amazon.titan-embed-text-v2:0\",
         \"arn:aws:bedrock:$REGION::foundation-model/amazon.nova-pro-v1:0\"]}
    ]}" >/dev/null
ROLE_ARN=$(aws iam get-role --role-name "$ROLE" --query Role.Arn --output text)
echo "role: $ROLE_ARN"

say "Building the Lambda bundle"
# provided.al2023 expects an executable named bootstrap.
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -tags lambda.norpc -o /tmp/bootstrap ./cmd/anchord
( cd /tmp && rm -f anchor.zip && zip -q anchor.zip bootstrap )
echo "bundle: $(du -h /tmp/anchor.zip | cut -f1)"

say "Deploying the function"
if aws lambda get-function --function-name "$FUNC" --region "$REGION" >/dev/null 2>&1; then
  aws lambda update-function-code --function-name "$FUNC" \
    --zip-file fileb:///tmp/anchor.zip --region "$REGION" >/dev/null
  aws lambda wait function-updated --function-name "$FUNC" --region "$REGION"
  aws lambda update-function-configuration --function-name "$FUNC" \
    --environment "Variables={ANCHOR_DB_SECRET=$SECRET}" \
    --timeout 30 --memory-size 512 --region "$REGION" >/dev/null
  echo "updated $FUNC"
else
  aws lambda create-function --function-name "$FUNC" \
    --runtime "$RUNTIME" --architectures arm64 --handler bootstrap \
    --role "$ROLE_ARN" --zip-file fileb:///tmp/anchor.zip \
    --environment "Variables={ANCHOR_DB_SECRET=$SECRET}" \
    --timeout 30 --memory-size 512 --region "$REGION" >/dev/null
  echo "created $FUNC"
fi
aws lambda wait function-updated --function-name "$FUNC" --region "$REGION"

say "Exposing a Function URL"
if ! aws lambda get-function-url-config --function-name "$FUNC" --region "$REGION" >/dev/null 2>&1; then
  aws lambda create-function-url-config --function-name "$FUNC" \
    --auth-type NONE --region "$REGION" >/dev/null
  # A Function URL with AuthType NONE still needs an explicit resource policy.
  aws lambda add-permission --function-name "$FUNC" \
    --statement-id anchor-public --action lambda:InvokeFunctionUrl \
    --principal '*' --function-url-auth-type NONE --region "$REGION" >/dev/null
fi
URL=$(aws lambda get-function-url-config --function-name "$FUNC" --region "$REGION" --query FunctionUrl --output text)

say "Verifying"
sleep 5
CODE=$(curl -s -o /dev/null -w '%{http_code}' "${URL}api/health" || true)
echo "health check: HTTP $CODE"

printf '\n\033[1;32mDEMO URL: %s\033[0m\n\n' "$URL"
echo "Logs:  aws logs tail /aws/lambda/$FUNC --follow --region $REGION"

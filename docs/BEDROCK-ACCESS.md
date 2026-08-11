# Bedrock model access

## The short version

There is no approval queue. Access to Bedrock foundation models is enabled by
default in all commercial regions, provided the calling identity has AWS
Marketplace permissions and the account has a valid payment method.

Two things actually gate us, and only one of them needs you:

| Model | What it needs |
|---|---|
| `amazon.titan-embed-text-v2:0` | Nothing. Amazon models are not sold through AWS Marketplace and have no product ID, so there is no subscription step. IAM permission to invoke is enough. |
| `amazon.nova-pro-v1:0` | Same. Nothing. |
| Anthropic Claude | A one-time First Time Use form, once per account or AWS Organization. **Access is granted immediately on submission.** It is not reviewed. |

So the only genuine gate is the Anthropic FTU form, and it clears the moment it
is submitted.

## Form values

Confirmed by Viraj on 2026-08-11: submit as an individual developer against the
public repository, not under an employer. AWS explicitly permits a GitHub or
project URL in place of a company website for individual developers.

```json
{
  "companyName": "Viraj Chogle",
  "companyWebsite": "https://github.com/virajchogle/anchor",
  "intendedUsers": "0",
  "industryOption": "Software",
  "otherIndustryOption": "",
  "useCases": "Anchor is an autonomous on-call agent for CockroachDB Cloud clusters, built as an individual entry for the CockroachDB x AWS Build with Agentic Memory hackathon. The model is used for incident diagnosis and remediation reasoning: given an incident symptom and similar past incidents retrieved from a vector index, it proposes a remediation action. Usage is internal to the developer, limited to demo and benchmark runs, and no end-user or third-party data is processed."
}
```

`intendedUsers` is `0`, meaning internal. That is accurate: the only user is the
developer running the demo, and the submission is a hackathon entry rather than a
deployed product with external users.

## What I will run once I have the key

Requires AWS CLI 2.27.42 or later. The local CLI is 2.36.20, so it is fine.

Submit the FTU form:

```sh
aws bedrock put-use-case-for-model-access \
  --form-data "$(printf '%s' "$FORM_JSON" | base64)"
```

where `FORM_JSON` is:

```json
{
  "companyName": "...",
  "companyWebsite": "...",
  "intendedUsers": "0",
  "industryOption": "Software",
  "otherIndustryOption": "",
  "useCases": "..."
}
```

Then create the agreement for the Anthropic model:

```sh
aws bedrock list-foundation-model-agreement-offers --model-id "$MODEL_ID"
aws bedrock create-foundation-model-agreement --model-id "$MODEL_ID" --offer-token "$OFFER_TOKEN"
```

Then verify, which is the only output that counts:

```sh
aws bedrock get-foundation-model-availability --model-id "$MODEL_ID"
```

A model is usable only when this reports:

```json
{
  "agreementAvailability": { "status": "AVAILABLE" },
  "authorizationStatus": "AUTHORIZED",
  "entitlementAvailability": "AVAILABLE",
  "regionAvailability": "AVAILABLE"
}
```

Gate 4 is not considered passed until I have that output for both the embedding
model and the reasoning model, plus a real round-trip invocation returning a
1024-dimension vector.

## If you would rather do it in the console

1. Sign in to https://console.aws.amazon.com/bedrock/ in **us-east-1**.
2. Left nav, under **Bedrock configurations**, choose **Model access**.
3. Choose **Modify model access**.
4. Check `Titan Text Embeddings V2`, an Anthropic Claude model, and
   `Nova Pro` as a fallback.
5. Choose **Next**. If Anthropic models are included you will be prompted to
   **Submit use case details**. Fill the form and submit.
6. Review the terms and **Submit**. Status becomes **Access granted**.

## Note on the IAM policy

The identity needs `aws-marketplace:Subscribe`, `aws-marketplace:Unsubscribe`,
and `aws-marketplace:ViewSubscriptions` for the first invocation of a
third-party model in the account. After the model is enabled once, invocation no
longer needs them. `PowerUserAccess` covers this during the build; the
least-privilege policy shipped in the README drops the Marketplace actions and
keeps only `bedrock:InvokeModel` scoped to the two model ARNs we actually use.

# Account setup checklist

Everything here is console-only work that requires your login. Do these in order.
Steps marked BLOCKING stop my progress until they are done.

When you finish, put the credentials in `~/.anchor/env` (outside this repo, which is
public). Do not paste them into chat and do not commit them. I will read that file.
The deployed system reads from AWS Secrets Manager instead; the local file exists
only so I can run the verification gates and integration tests.

---

## 1. CockroachDB Cloud (BLOCKING)

1. Sign up at https://cockroachlabs.cloud.
2. Check the hackathon page for a credit or promo code and apply it before creating
   the cluster. Cluster tier decides three things I need to design around, so it is
   worth spending the credit on the higher tier if the code allows it.
3. Create a cluster:
   - Cloud provider: **AWS**
   - Region: **us-east-1**
   - Tier: **Standard** if credits allow, otherwise Basic.
   Region matters. Put this in the same region as AWS so the benchmark latency
   numbers measure our design and not a cross-region hop.
4. Create a SQL user for me and save the password.
5. Download the CA cert if the console offers one, and copy the full connection
   string. It must contain `sslmode=verify-full`.
6. Go to **Access Management > Service Accounts**, create a service account named
   `anchor-agent`, and give it cluster administration rights. Create an API key for
   it and copy the secret immediately, because it is shown only once.
7. Look for a **Managed MCP Server** option on the cluster page and enable it if
   present. Tell me whether you see it at all, since its availability may be tier
   dependent.

Tell me: the tier you created, and whether the MCP server option existed.

## 2. AWS (BLOCKING)

1. Sign up at https://aws.amazon.com if you do not have an account. Requires a
   credit card even though our usage should stay inside the free tier.
2. Confirm a valid payment method is attached, under **Billing > Payment
   preferences**. AWS Marketplace subscriptions fail without one, and Bedrock
   third-party model access goes through Marketplace.
3. Create an IAM user with programmatic access (access key plus secret). For the
   build phase attach `PowerUserAccess`. I will replace this with a
   least-privilege policy before submission, and the README will show the
   tightened policy rather than the broad one.
4. Set your region to **us-east-1** and keep it there. Titan Text Embeddings V2
   is only available in us-east-1 and us-west-2.

That is all you need to do for AWS. Bedrock model access is **not** a review
queue, and I can drive the rest from the CLI once I have the key. See
`docs/BEDROCK-ACCESS.md` for what I will run and the one piece of input I need
from you.

## 3. GitHub

1. Create a public repository named `anchor`.
2. Leave it empty. No README, no license, no gitignore. I will push the initial
   commit and set the About section, which is where the judges check for the
   MIT license.

Tell me: the repository URL.

---

## Credentials file

Create `~/.anchor/env` with this shape and `chmod 600` it:

```sh
export ANCHOR_DB_URL='postgresql://USER:PASSWORD@HOST:26257/anchor?sslmode=verify-full'
export CCLOUD_API_KEY='...'
export CCLOUD_CLUSTER_ID='...'
export AWS_ACCESS_KEY_ID='...'
export AWS_SECRET_ACCESS_KEY='...'
export AWS_REGION='us-east-1'
```

`~/.anchor/` is outside the repository, and the repository `.gitignore` also blocks
`*.env` and `.env*` as a second layer of protection.

# gdax_bookorder_avg

This go program listens to a Kinesis `gdax_order_price_avg` stream that is being written to by AWS Data Analytics app which is being fed by `gdax-websocket` ([gdax_websocket](https://github.com/wazupwiddat/gdax_websocket))

Need to have a AWS Account (Free Tier should work fine).

To setup AWS account:

1. Setup your AWS account [AWS](https://aws.amazon.com/)
2. Create Access Key [Security Credentials](https://console.aws.amazon.com/iam/home?region=us-east-1#/security_credential)
3. Note credentials profile

To run:

1. Clone this repository into your local go/src directory.
2. Switch to the gdax_bookorder_avg directory
3. Make files executable: `chmod u+x *.go`
4. Grab dependencies: `go get ./...`
5. Build: `go build`

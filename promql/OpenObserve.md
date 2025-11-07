### Running the promql compliance suite locally.

- Clone and compile promql-compliance-tester
  - clone the repo - https://github.com/prometheus/compliance
  - `cd promql && go build ./cmd/promql-compliance-tester`
- Ingest data into both O2 and prometheus
  - Run your local instance of O2 as usual
  - Run prometheus docker container that writes metrics data into both O2 and prometheus
    - Modify `prometheus-test-data.yml` **remote-write url** to your locally running O2 metrics ingestion url
    - `docker-compose up -d` to start off prometheus docker container
      - you might want to modify the `ports`, `network_mode`, and `extra_hosts` section according to your docker setup
  - Leave the container running for about an hour or so to collect enough data
- Run the compliance test
  - In this repo, we have `promql-test-queries.yml`, which includes all the test queries
  - After enough data is ingested, we can run the `promql-compliance-tester` that was complied in the first step with
    `./promql-compliance-tester -config-file ./test-openobserve.yml -config-file ./promql-test-queries.yml` and 
    `./promql-compliance-tester-modify -config-file ./test-openobserve.yml -config-file ./promql-test-queries.yml`

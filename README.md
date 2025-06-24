# TA_GEAMAN
Generic test automation framework - Support for starting, queueing and stopping test suites - Collects results and stores them - To be displayed in a dashboard 



---windows
go build -o runner.exe ./automation/runner.go
go build -o test-server.exe ./main.go
----macos
go build -o ./bin/runner ./automation/runner.go

docker-compose up --build --scale runner=3 ||||| - everything + 3 instances of runnes 
docker-compose up --build server ( -d for detached ) ||||  - for central server
docker-compose up -d postgres minio rabbitmq ||||  -without central server 

database
docker exec -it ta_geaman-postgres-1 psql -U user -d test_results_db  

docker build -t ta-geaman-runner -f Dockerfile.runner .

docker run --rm -it ^
  -e RUNNER_ID="standalone-docker-runner-01" ^
  -e API_BASE_URL="https://host.docker.internal:8443/api/v1" ^
  -e ASSIGNED_PROJECTS="VPN_Desktop,AnotherProject" ^
  -e POLLING_INTERVAL_SECONDS="10" ^
  ta-geaman-runner


 --scale runner=3 - to scale


 flutter run -d chrome 

 RUNNER_ID="native-runner-02" ./bin/runner    - set id 

docker  build -t playwright_runner_two -f Dockerfile.containerized-runner-web .

docker run --rm --ipc=host --network="host" -e RUNNER_ID="runner-002-macos-webApp" -e API_BASE_URL="https://localhost:8443/api/v1" -e ASSIGNED_PROJECTS="webApp" -e CONTAINER="true"   playwright_runner_two

docker run --rm --ipc=host --network="host" -e RUNNER_ID="runner-003-macos-webApp" -e API_BASE_URL="https://localhost:8443/api/v1" -e ASSIGNED_PROJECTS="webApp" -e CONTAINER="true"   playwright_runner_two

docker run --rm --ipc=host --network="host" -e RUNNER_ID="runner-004-macos-webApp" -e API_BASE_URL="https://localhost:8443/api/v1" -e ASSIGNED_PROJECTS="webApp" -e CONTAINER="true"   playwright_runner_two



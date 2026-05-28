# TODO - Ensure openapi tools are installed:
# npm install @openapitools/openapi-generator-cli -g
openapi-generator-cli generate \
    -i ../../_pipedrive-api.yaml \
    -g go \
    -o ../../ \
    -c config.yaml \
    --skip-validate-spec \
    --git-user-id ai-connor \
    --git-repo-id pipedrive-api

cd ../..
go fmt
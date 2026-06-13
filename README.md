# Teamail

Grant read-only access to mailboxes in a browser. Configure the mailboxes you want to give read access to and then configure the users and give them access to only the mailboxes they should have access to. All done via one YAML file.

WARNING! This piece of software was built with the help of AI.

## How to run

```sh
docker run -v ./config.yaml:/config.yaml:ro -p 8080:8080 ghcr.io/tibeer/teamail:latest
```

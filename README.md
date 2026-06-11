# Runner 🚀

Runner is a reverse proxy that keeps your most recent version with zero downtime.

Binaries are downloaded from Github Actions in the repository and spawn a process
in an specific port with --port flag - this is a requirement for the application.

## Configuration

Place a `config.json` file in the root directory:

```json
{
  "apps": [
    {
      "externalPort": 8000,
      "repo": "hesenger/pethello",
      "binName": "pethello",
      "token": "ghp_yourPersonalAccessToken"
    }
  ]
}
```

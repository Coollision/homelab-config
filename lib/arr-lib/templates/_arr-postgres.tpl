{{- define "arr-lib.arrPostgresInitContainer" -}}
- name: {{ include "helm.fullname" . }}-wait-postgres
  image: {{ .Values.arrXmlPostgres.waitImage | default "postgres:16-alpine" }}
  imagePullPolicy: {{ .Values.arrXmlPostgres.waitImagePullPolicy | default "IfNotPresent" }}
  command:
    - /bin/sh
    - -ec
    - |
      deadline=$(( $(date +%s) + {{ .Values.arrXmlPostgres.waitTimeoutSeconds | default 900 }} ))

      wait_for_db() {
        db_name="$1"
        while true; do
          if PGPASSWORD="$POSTGRES_PASSWORD" psql \
            -h "$POSTGRES_HOST" \
            -p "$POSTGRES_PORT" \
            -U "$POSTGRES_USER" \
            -d "$db_name" \
            -c 'SELECT 1;' >/dev/null 2>&1; then
            echo "Postgres bootstrap ready for database: $db_name"
            break
          fi

          if [ "$(date +%s)" -ge "$deadline" ]; then
            echo "Timed out waiting for Postgres bootstrap on database: $db_name"
            exit 1
          fi

          echo "Waiting for Postgres bootstrap on database: $db_name"
          sleep 5
        done
      }

      wait_for_db "$POSTGRES_MAIN_DB"
      wait_for_db "$POSTGRES_LOG_DB"
  env:
    - name: POSTGRES_USER
      valueFrom:
        secretKeyRef:
          name: {{ .Values.arrXmlPostgres.secretName | default "services-db" }}
          key: {{ required "arrXmlPostgres.userKey is required" .Values.arrXmlPostgres.userKey }}
    - name: POSTGRES_PASSWORD
      valueFrom:
        secretKeyRef:
          name: {{ .Values.arrXmlPostgres.secretName | default "services-db" }}
          key: {{ required "arrXmlPostgres.passwordKey is required" .Values.arrXmlPostgres.passwordKey }}
    - name: POSTGRES_HOST
      valueFrom:
        configMapKeyRef:
          name: {{ .Values.arrXmlPostgres.configMapName | default "service-db" }}
          key: {{ .Values.arrXmlPostgres.hostKey | default "host" }}
    - name: POSTGRES_PORT
      valueFrom:
        configMapKeyRef:
          name: {{ .Values.arrXmlPostgres.configMapName | default "service-db" }}
          key: {{ .Values.arrXmlPostgres.portKey | default "port" }}
    - name: POSTGRES_MAIN_DB
      valueFrom:
        configMapKeyRef:
          name: {{ .Values.arrXmlPostgres.configMapName | default "service-db" }}
          key: {{ required "arrXmlPostgres.mainDbKey is required" .Values.arrXmlPostgres.mainDbKey }}
    - name: POSTGRES_LOG_DB
      valueFrom:
        configMapKeyRef:
          name: {{ .Values.arrXmlPostgres.configMapName | default "service-db" }}
          key: {{ required "arrXmlPostgres.logDbKey is required" .Values.arrXmlPostgres.logDbKey }}
- name: {{ include "helm.fullname" . }}-postgres-config
  image: {{ .Values.arrXmlPostgres.image | default (include "shared-lib.image" .) }}
  imagePullPolicy: {{ .Values.arrXmlPostgres.imagePullPolicy | default (include "shared-lib.imagePullPolicy" . | trimAll "\"") }}
  command:
    - /bin/sh
    - -ec
    - |
      file="{{ .Values.arrXmlPostgres.configFile | default "/config/config.xml" }}"
      if [ ! -f "$file" ]; then
        {{- if .Values.arrXmlPostgres.requireExistingConfig }}
        echo "config.xml not found, blocking startup until config is present"
        exit 1
        {{- else }}
        echo "config.xml not found yet, skipping Postgres bootstrap"
        exit 0
        {{- end }}
      fi

      upsert() {
        key="$1"
        value="$2"
        if xmlstarlet sel -Q -t -c "/Config/${key}" "$file" >/dev/null 2>&1; then
          xmlstarlet ed -L -u "/Config/${key}" -v "$value" "$file"
        else
          xmlstarlet ed -L -s "/Config" -t elem -n "$key" -v "$value" "$file"
        fi
      }

      upsert PostgresUser "$POSTGRES_USER"
      upsert PostgresPassword "$POSTGRES_PASSWORD"
      upsert PostgresPort "$POSTGRES_PORT"
      upsert PostgresHost "$POSTGRES_HOST"
      upsert PostgresMainDb "$POSTGRES_MAIN_DB"
      upsert PostgresLogDb "$POSTGRES_LOG_DB"
  env:
    - name: POSTGRES_USER
      valueFrom:
        secretKeyRef:
          name: {{ .Values.arrXmlPostgres.secretName | default "services-db" }}
          key: {{ required "arrXmlPostgres.userKey is required" .Values.arrXmlPostgres.userKey }}
    - name: POSTGRES_PASSWORD
      valueFrom:
        secretKeyRef:
          name: {{ .Values.arrXmlPostgres.secretName | default "services-db" }}
          key: {{ required "arrXmlPostgres.passwordKey is required" .Values.arrXmlPostgres.passwordKey }}
    - name: POSTGRES_HOST
      valueFrom:
        configMapKeyRef:
          name: {{ .Values.arrXmlPostgres.configMapName | default "service-db" }}
          key: {{ .Values.arrXmlPostgres.hostKey | default "host" }}
    - name: POSTGRES_PORT
      valueFrom:
        configMapKeyRef:
          name: {{ .Values.arrXmlPostgres.configMapName | default "service-db" }}
          key: {{ .Values.arrXmlPostgres.portKey | default "port" }}
    - name: POSTGRES_MAIN_DB
      valueFrom:
        configMapKeyRef:
          name: {{ .Values.arrXmlPostgres.configMapName | default "service-db" }}
          key: {{ required "arrXmlPostgres.mainDbKey is required" .Values.arrXmlPostgres.mainDbKey }}
    - name: POSTGRES_LOG_DB
      valueFrom:
        configMapKeyRef:
          name: {{ .Values.arrXmlPostgres.configMapName | default "service-db" }}
          key: {{ required "arrXmlPostgres.logDbKey is required" .Values.arrXmlPostgres.logDbKey }}
  volumeMounts:
    - name: {{ printf "%s-%s" (include "helm.fullname" .) (.Values.arrXmlPostgres.configStorageNameSuffix | default "config") | trunc 63 | trimSuffix "-" }}
      mountPath: {{ .Values.arrXmlPostgres.configMountPath | default "/config" }}
{{- end }}

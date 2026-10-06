ARG BASE
FROM ${BASE} AS worker
USER 0
COPY service-firewall.sh firewall.sh job-disk-init.py /opt/ci/
COPY ci-service.py /usr/local/bin/ci-service
COPY consumer /consumer
RUN chmod 755 /opt/ci/*.sh /usr/local/bin/ci-service && chmod 444 /consumer/catalog.json /consumer/policy.json
ENTRYPOINT []
USER 1001
CMD ["sleep", "600"]

FROM worker AS harness
USER 0
COPY check /service-check
ENTRYPOINT ["/service-check"]

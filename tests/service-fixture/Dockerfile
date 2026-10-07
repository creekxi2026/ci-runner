ARG BASE
FROM ${BASE}
USER 0
COPY service-firewall.sh firewall.sh job-disk-init.py /opt/ci/
COPY ci-service.py /usr/local/bin/ci-service
COPY tests/service-fixture/adapter.py /ci-adapter
COPY tests/service-fixture/server.py /ci-server
RUN chmod 755 /opt/ci/*.sh /usr/local/bin/ci-service /ci-adapter /ci-server
CMD ["python3", "/ci-server"]

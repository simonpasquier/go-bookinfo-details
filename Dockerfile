FROM scratch
COPY ./details /
ENTRYPOINT [ "/details" ]

CREATE TABLE views
(
    id BIGINT PRIMARY KEY,
    url_id BIGINT NOT NULL,
    client_id BIGINT NOT NULL,

    created_at BIGINT NOT NULL
) ENGINE = InnoDB;

CREATE INDEX views_url_id_idx ON views(url_id);

CREATE TABLE view_counts
(
    id BIGINT PRIMARY KEY,
    url_id BIGINT NOT NULL,
    count BIGINT NOT NULL,

    updated_at BIGINT NOT NULL
) ENGINE = InnoDB;

CREATE INDEX view_counts_url_id_idx ON view_counts(url_id);

CREATE TABLE likes
(
    id BIGINT PRIMARY KEY,
    url_id BIGINT NOT NULL,
    client_id BIGINT NOT NULL,

    created_at BIGINT NOT NULL
) ENGINE = InnoDB;

CREATE INDEX likes_url_id_idx ON likes(url_id);
CREATE UNIQUE INDEX likes_url_id_client_id_idx ON likes(url_id, client_id);

CREATE TABLE like_counts
(
    id BIGINT PRIMARY KEY,
    url_id BIGINT NOT NULL,
    count BIGINT NOT NULL,

    updated_at BIGINT NOT NULL
) ENGINE = InnoDB;

CREATE INDEX like_counts_url_id_idx ON like_counts(url_id);

CREATE TABLE client_identifiers
(
    id BIGINT PRIMARY KEY,
    token TEXT NOT NULL,

    created_at BIGINT NOT NULL
) ENGINE = InnoDB;

CREATE INDEX client_identifiers_token_idx ON client_identifiers(token);

CREATE TABLE client_fingerprints
(
    id BIGINT PRIMARY KEY,
    client_id BIGINT NOT NULL,

    user_agent TEXT NOT NULL,
    user_agent_data TEXT NOT NULL,
    screen_width BIGINT NOT NULL,
    screen_height BIGINT NOT NULL,
    fpversion INT NOT NULL,
    fphash TEXT NOT NULL,

    created_at BIGINT NOT NULL
) ENGINE = InnoDB;

CREATE INDEX client_fingerprints_client_id_idx ON client_fingerprints(client_id);
CREATE INDEX client_fingerprints_fphash_idx ON client_fingerprints(fphash);

CREATE TABLE urls
(
    id BIGINT PRIMARY KEY,
    url TEXT NOT NULL,

    created_at BIGINT NOT NULL
) ENGINE = InnoDB;

CREATE INDEX urls_id_idx ON urls(id);

CREATE TABLE randflake_leases
(
    uuid BINARY(16) PRIMARY KEY,
    node_id BIGINT NOT NULL,
    created_at BIGINT NOT NULL,
    expires_at BIGINT NOT NULL
) ENGINE = InnoDB;

CREATE UNIQUE INDEX randflake_leases_node_id_idx ON randflake_leases(node_id);
CREATE INDEX randflake_leases_expires_at_idx ON randflake_leases(expires_at ASC);

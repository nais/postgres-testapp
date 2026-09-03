CREATE TABLE app_control (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    running boolean NOT NULL DEFAULT false
);

INSERT INTO app_control (singleton, running) VALUES (true, false);

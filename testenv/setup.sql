CREATE DATABASE IF NOT EXISTS emr_ch;

CREATE TABLE IF NOT EXISTS emr_ch.tbl_polygons_bmt (
  key MultiPolygon, filial Int8, avtocod Int8, bmt UInt64,
  filial_name LowCardinality(String), avtocod_name LowCardinality(String), bmt_name LowCardinality(String)
) ENGINE = TinyLog;

INSERT INTO emr_ch.tbl_polygons_bmt SELECT
  [[[ (37.5 + 0.02*intDiv(number,10), 55.7 + 0.02*(number%10)),
      (37.51 + 0.02*intDiv(number,10), 55.7 + 0.02*(number%10)),
      (37.51 + 0.02*intDiv(number,10), 55.71 + 0.02*(number%10)),
      (37.5 + 0.02*intDiv(number,10), 55.71 + 0.02*(number%10)),
      (37.5 + 0.02*intDiv(number,10), 55.7 + 0.02*(number%10)) ]]],
  toInt8(number % 3 + 1), toInt8(number % 5 + 1), number + 1000,
  concat('Филиал ', toString(number % 3 + 1)),
  concat('Автоколонна ', toString(number % 5 + 1)),
  concat('БМТ участок ', toString(number + 1))
FROM numbers(200);

CREATE TABLE IF NOT EXISTS emr_ch.tbl_zones (
  geom MultiPolygon, zone_name String, zone_type LowCardinality(String),
  area_km2 Float64, active UInt8
) ENGINE = TinyLog;

INSERT INTO emr_ch.tbl_zones SELECT
  [[[ (37.15 + 0.025*intDiv(number,8), 55.6 + 0.025*(number%8)),
      (37.165 + 0.025*intDiv(number,8), 55.6 + 0.025*(number%8)),
      (37.165 + 0.025*intDiv(number,8), 55.615 + 0.025*(number%8)),
      (37.15 + 0.025*intDiv(number,8), 55.615 + 0.025*(number%8)),
      (37.15 + 0.025*intDiv(number,8), 55.6 + 0.025*(number%8)) ]]],
  concat('Зона обслуживания ', toString(number + 1)),
  ['жилая','промышленная','парковая'][number % 3 + 1],
  round(rand()/4294967295*20, 2),
  number % 2
FROM numbers(80);

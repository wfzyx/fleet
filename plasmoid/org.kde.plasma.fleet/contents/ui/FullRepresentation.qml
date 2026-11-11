pragma ComponentBehavior: Bound

import QtQuick
import QtQuick.Layouts
import QtQuick.Controls as QQC2
import org.kde.plasma.plasmoid
import org.kde.plasma.core as PlasmaCore
import org.kde.plasma.components as PlasmaComponents3
import org.kde.plasma.extras as PlasmaExtras
import org.kde.kirigami as Kirigami

PlasmaExtras.Representation {
    id: fullRoot

    Layout.minimumWidth: Kirigami.Units.gridUnit * 22
    Layout.minimumHeight: Kirigami.Units.gridUnit * 18
    Layout.preferredWidth: Kirigami.Units.gridUnit * 24
    Layout.preferredHeight: Kirigami.Units.gridUnit * 22

    collapseMarginsHint: true

    header: PlasmaExtras.PlasmoidHeading {
        contentItem: RowLayout {
            spacing: Kirigami.Units.smallSpacing

            Kirigami.Icon {
                source: "applications-development"
                Layout.preferredWidth: Kirigami.Units.iconSizes.smallMedium
                Layout.preferredHeight: Layout.preferredWidth
            }

            Kirigami.Heading {
                text: "Fleet Cockpit"
                level: 3
                Layout.fillWidth: true
            }

            // Online / Offline Status Badge
            Rectangle {
                Layout.preferredHeight: Kirigami.Units.gridUnit * 1.1
                Layout.preferredWidth: statusLabel.implicitWidth + Kirigami.Units.gridUnit
                radius: height / 2
                color: root.fleetStatus.online ? Kirigami.Theme.positiveBackgroundColor : Kirigami.Theme.backgroundColor
                border.color: root.fleetStatus.online ? Kirigami.Theme.positiveTextColor : Kirigami.Theme.disabledTextColor
                border.width: 1

                PlasmaComponents3.Label {
                    id: statusLabel
                    anchors.centerIn: parent
                    text: root.fleetStatus.online ? "Online" : "Offline"
                    font.bold: true
                    font.pixelSize: Kirigami.Theme.smallFont.pixelSize
                    color: root.fleetStatus.online ? Kirigami.Theme.positiveTextColor : Kirigami.Theme.disabledTextColor
                }
            }

            PlasmaComponents3.ToolButton {
                icon.name: "view-refresh"
                onClicked: root.reloadStatus()
                QQC2.ToolTip.text: "Refresh status"
                QQC2.ToolTip.visible: hovered
            }
        }
    }

    contentItem: ColumnLayout {
        spacing: Kirigami.Units.smallSpacing

        // Summary Bar: Spend & Active Workers
        Rectangle {
            Layout.fillWidth: true
            Layout.preferredHeight: Kirigami.Units.gridUnit * 2.8
            color: Kirigami.Theme.backgroundColor
            radius: Kirigami.Units.smallSpacing
            border.color: Kirigami.Theme.disabledTextColor
            border.width: 1
            opacity: 0.95

            RowLayout {
                anchors.fill: parent
                anchors.margins: Kirigami.Units.smallSpacing
                spacing: Kirigami.Units.largeSpacing

                ColumnLayout {
                    spacing: 1
                    PlasmaComponents3.Label {
                        text: "ACTIVE SPEND"
                        font.pixelSize: Kirigami.Theme.smallFont.pixelSize
                        color: Kirigami.Theme.disabledTextColor
                        font.bold: true
                    }
                    PlasmaComponents3.Label {
                        text: "$" + (root.fleetStatus.total_spend_usd || 0).toFixed(2)
                        font.bold: true
                        color: Kirigami.Theme.positiveTextColor
                        font.pixelSize: Kirigami.Theme.defaultFont.pixelSize
                    }
                }

                Rectangle {
                    Layout.fillHeight: true
                    Layout.preferredWidth: 1
                    color: Kirigami.Theme.disabledTextColor
                    opacity: 0.3
                }

                ColumnLayout {
                    spacing: 1
                    PlasmaComponents3.Label {
                        text: "WORKERS"
                        font.pixelSize: Kirigami.Theme.smallFont.pixelSize
                        color: Kirigami.Theme.disabledTextColor
                        font.bold: true
                    }
                    PlasmaComponents3.Label {
                        text: (root.fleetStatus.working_count || 0) + " running"
                        font.bold: true
                        color: (root.fleetStatus.working_count || 0) > 0 ? Kirigami.Theme.highlightColor : Kirigami.Theme.textColor
                    }
                }

                Rectangle {
                    Layout.fillHeight: true
                    Layout.preferredWidth: 1
                    color: Kirigami.Theme.disabledTextColor
                    opacity: 0.3
                }

                ColumnLayout {
                    spacing: 1
                    PlasmaComponents3.Label {
                        text: "REVIEW"
                        font.pixelSize: Kirigami.Theme.smallFont.pixelSize
                        color: Kirigami.Theme.disabledTextColor
                        font.bold: true
                    }
                    PlasmaComponents3.Label {
                        text: (root.fleetStatus.review_count || 0) + " pending"
                        font.bold: true
                        color: (root.fleetStatus.review_count || 0) > 0 ? Kirigami.Theme.positiveTextColor : Kirigami.Theme.textColor
                    }
                }
            }
        }

        // Section Title: Spaces
        PlasmaComponents3.Label {
            text: "SPACES"
            font.bold: true
            font.pixelSize: Kirigami.Theme.smallFont.pixelSize
            color: Kirigami.Theme.disabledTextColor
            Layout.topMargin: Kirigami.Units.smallSpacing
        }

        // Spaces Repeater
        ColumnLayout {
            Layout.fillWidth: true
            spacing: Kirigami.Units.smallSpacing

            Repeater {
                model: root.fleetStatus.spaces || []
                delegate: Rectangle {
                    required property var modelData
                    Layout.fillWidth: true
                    Layout.preferredHeight: Kirigami.Units.gridUnit * 2.2
                    color: Kirigami.Theme.alternateBackgroundColor
                    radius: Kirigami.Units.smallSpacing
                    border.color: Kirigami.Theme.disabledTextColor
                    border.width: 1
                    opacity: 0.9

                    RowLayout {
                        anchors.fill: parent
                        anchors.margins: Kirigami.Units.smallSpacing

                        Kirigami.Icon {
                            source: "folder-git"
                            Layout.preferredWidth: Kirigami.Units.iconSizes.small
                            Layout.preferredHeight: Layout.preferredWidth
                        }

                        PlasmaComponents3.Label {
                            text: modelData.name || modelData.id
                            font.bold: true
                            Layout.fillWidth: true
                        }

                        PlasmaComponents3.Label {
                            text: "Lead: " + (modelData.lead || "offline")
                            font.pixelSize: Kirigami.Theme.smallFont.pixelSize
                            color: modelData.lead === "idle" ? Kirigami.Theme.positiveTextColor : modelData.lead === "working" ? Kirigami.Theme.highlightColor : Kirigami.Theme.disabledTextColor
                        }

                        PlasmaComponents3.Label {
                            text: "$" + (modelData.spend_usd || 0).toFixed(2) + " / " + (modelData.max_budget_usd > 0 ? "$" + modelData.max_budget_usd.toFixed(2) : "∞")
                            font.family: "Monospace"
                            font.pixelSize: Kirigami.Theme.smallFont.pixelSize
                        }
                    }
                }
            }
        }

        // Section Title: Tasks
        PlasmaComponents3.Label {
            text: "TASKS"
            font.bold: true
            font.pixelSize: Kirigami.Theme.smallFont.pixelSize
            color: Kirigami.Theme.disabledTextColor
            Layout.topMargin: Kirigami.Units.smallSpacing
        }

        // Tasks List
        PlasmaComponents3.ScrollView {
            Layout.fillWidth: true
            Layout.fillHeight: true

            ListView {
                id: taskListView
                model: root.fleetStatus.tasks || []
                clip: true
                spacing: Kirigami.Units.smallSpacing

                delegate: Rectangle {
                    required property var modelData
                    width: taskListView.width
                    height: Kirigami.Units.gridUnit * 3.2
                    color: Kirigami.Theme.alternateBackgroundColor
                    radius: Kirigami.Units.smallSpacing
                    border.color: modelData.status === "review" ? Kirigami.Theme.positiveTextColor : modelData.status === "working" ? Kirigami.Theme.highlightColor : Kirigami.Theme.disabledTextColor
                    border.width: 1

                    RowLayout {
                        anchors.fill: parent
                        anchors.margins: Kirigami.Units.smallSpacing

                        ColumnLayout {
                            Layout.fillWidth: true
                            spacing: 2

                            RowLayout {
                                spacing: Kirigami.Units.smallSpacing
                                Rectangle {
                                    Layout.preferredHeight: Kirigami.Units.gridUnit
                                    Layout.preferredWidth: statusTag.implicitWidth + 8
                                    radius: 3
                                    color: modelData.status === "review" ? Kirigami.Theme.positiveBackgroundColor : modelData.status === "working" ? Kirigami.Theme.highlightBackgroundColor : Kirigami.Theme.backgroundColor

                                    PlasmaComponents3.Label {
                                        id: statusTag
                                        anchors.centerIn: parent
                                        text: (modelData.status || "queued").toUpperCase()
                                        font.bold: true
                                        font.pixelSize: Kirigami.Theme.smallFont.pixelSize
                                        color: modelData.status === "review" ? Kirigami.Theme.positiveTextColor : modelData.status === "working" ? Kirigami.Theme.highlightTextColor : Kirigami.Theme.textColor
                                    }
                                }

                                PlasmaComponents3.Label {
                                    text: modelData.id
                                    font.bold: true
                                    font.family: "Monospace"
                                }
                            }

                            PlasmaComponents3.Label {
                                text: modelData.title || "No description"
                                elide: Text.ElideRight
                                Layout.fillWidth: true
                                font.pixelSize: Kirigami.Theme.smallFont.pixelSize
                                color: Kirigami.Theme.disabledTextColor
                            }
                        }

                        // Quick Action Button if review requested
                        PlasmaComponents3.Button {
                            visible: modelData.status === "review"
                            text: "Review"
                            icon.name: "document-edit"
                            onClicked: {
                                var req = new XMLHttpRequest();
                                req.open("POST", "http://127.0.0.1:2978/action?type=review&task=" + encodeURIComponent(modelData.id));
                                req.send();
                            }
                        }

                        PlasmaComponents3.Label {
                            text: "$" + (modelData.spend_usd || 0).toFixed(2)
                            font.family: "Monospace"
                            font.bold: true
                        }
                    }
                }

                PlasmaExtras.PlaceholderMessage {
                    anchors.centerIn: parent
                    visible: taskListView.count === 0
                    iconName: "task-complete"
                    text: "No active tasks in progress"
                }
            }
        }

        // Footer: Quick Actions
        RowLayout {
            Layout.fillWidth: true
            spacing: Kirigami.Units.smallSpacing

            PlasmaComponents3.Button {
                Layout.fillWidth: true
                text: "Open Fleet Cockpit (Kitty)"
                icon.name: "utilities-terminal"
                onClicked: {
                    var req = new XMLHttpRequest();
                    req.open("POST", "http://127.0.0.1:2978/action?type=terminal");
                    req.send();
                }
            }
        }
    }
}
